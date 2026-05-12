package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/AitorConS/unikernel-engine/internal/autotls"
	"github.com/AitorConS/unikernel-engine/internal/image"
	"github.com/AitorConS/unikernel-engine/internal/ociblob"
	"github.com/AitorConS/unikernel-engine/internal/registry"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		addr        string
		storePath   string
		token       string
		jwtSecret   string
		jwtIssuer   string
		jwtAudience string
		tlsCert     string
		tlsKey      string
		noAutoTLS   bool
	)
	root := &cobra.Command{
		Use:     "unireg",
		Short:   "UniKernel standalone registry server",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), addr, storePath, token, jwtSecret, jwtIssuer, jwtAudience, tlsCert, tlsKey, noAutoTLS)
		},
	}
	root.Flags().StringVar(&addr, "addr", ":5000", "listen address for the registry server")
	root.Flags().StringVar(&storePath, "store", defaultStorePath(), "local image store directory")
	root.Flags().StringVar(&token, "token", os.Getenv("UNI_REGISTRY_TOKEN"),
		"bearer token for registry auth (or set UNI_REGISTRY_TOKEN)")
	root.Flags().StringVar(&jwtSecret, "jwt-secret", os.Getenv("UNI_REGISTRY_JWT_SECRET"),
		"JWT HMAC secret for scoped auth (or set UNI_REGISTRY_JWT_SECRET)")
	root.Flags().StringVar(&jwtIssuer, "jwt-issuer", os.Getenv("UNI_REGISTRY_JWT_ISSUER"),
		"expected JWT issuer (or set UNI_REGISTRY_JWT_ISSUER)")
	root.Flags().StringVar(&jwtAudience, "jwt-audience", os.Getenv("UNI_REGISTRY_JWT_AUDIENCE"),
		"expected JWT audience (or set UNI_REGISTRY_JWT_AUDIENCE)")
	root.Flags().StringVar(&tlsCert, "tls-cert", os.Getenv("UNI_REGISTRY_TLS_CERT"),
		"TLS certificate file (or set UNI_REGISTRY_TLS_CERT)")
	root.Flags().StringVar(&tlsKey, "tls-key", os.Getenv("UNI_REGISTRY_TLS_KEY"),
		"TLS private key file (or set UNI_REGISTRY_TLS_KEY)")
	root.Flags().BoolVar(&noAutoTLS, "no-auto-tls", false, "disable auto-generated self-signed TLS cert")
	root.AddCommand(newGCCmd())
	return root
}

func newGCCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage collect unreferenced registry blobs",
		RunE: func(_ *cobra.Command, _ []string) error {
			blobStore, err := ociblob.NewStore(blobsDir())
			if err != nil {
				return fmt.Errorf("unireg gc: blob store: %w", err)
			}
			ociStore, err := registry.NewOCIStore(ociDir())
			if err != nil {
				return fmt.Errorf("unireg gc: OCI store: %w", err)
			}
			result, err := registry.GarbageCollect(blobStore, ociStore)
			if err != nil {
				return fmt.Errorf("unireg gc: %w", err)
			}
			slog.Info("registry gc complete", "removed", result.Removed, "kept", result.Kept)
			return nil
		},
	}
	return cmd
}

func serve(ctx context.Context, addr, storePath, token, jwtSecret, jwtIssuer, jwtAudience, tlsCert, tlsKey string, noAutoTLS bool) error {
	if err := validateTLSConfig(tlsCert, tlsKey); err != nil {
		return fmt.Errorf("unireg: TLS config: %w", err)
	}

	useTLS := false
	if strings.TrimSpace(tlsCert) != "" {
		useTLS = true
	} else if !noAutoTLS {
		selfSignedCert := filepath.Join(autotls.DefaultCertDir(), "cert.pem")
		selfSignedKey := filepath.Join(autotls.DefaultCertDir(), "key.pem")
		_, err := autotls.EnsureCert(selfSignedCert, selfSignedKey)
		if err != nil {
			return fmt.Errorf("unireg: generate self-signed cert: %w", err)
		}
		slog.Info("using auto-generated self-signed TLS certificate", "cert", selfSignedCert)
		tlsCert = selfSignedCert
		tlsKey = selfSignedKey
		useTLS = true
	}

	imgStore, err := image.NewStore(storePath)
	if err != nil {
		return fmt.Errorf("unireg: image store: %w", err)
	}
	blobStore, err := ociblob.NewStore(blobsDir())
	if err != nil {
		return fmt.Errorf("unireg: blob store: %w", err)
	}
	ociStore, err := registry.NewOCIStore(ociDir())
	if err != nil {
		return fmt.Errorf("unireg: OCI store: %w", err)
	}

	opts := []registry.Option{registry.WithBlobStore(blobStore), registry.WithOCIStore(ociStore)}
	if token != "" {
		opts = append(opts, registry.WithBearerToken(token, "uni-registry"))
	}
	if jwtSecret != "" {
		opts = append(opts, registry.WithJWTAuth(jwtSecret, "uni-registry"))
		opts = append(opts, registry.WithJWTValidation(jwtIssuer, jwtAudience))
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: registry.NewServer(imgStore, opts...).Handler(),
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		slog.Info("unireg listening", "addr", addr, "tls", useTLS)
		var err error
		if useTLS {
			err = srv.ListenAndServeTLS(tlsCert, tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			slog.Error("unireg server", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			slog.Warn("unireg shutdown", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("unireg shutdown complete")
	return nil
}

func validateTLSConfig(certPath, keyPath string) error {
	cert := strings.TrimSpace(certPath)
	key := strings.TrimSpace(keyPath)
	if cert == "" && key == "" {
		return nil
	}
	if cert == "" || key == "" {
		return fmt.Errorf("both TLS cert and key are required when one is specified")
	}
	return nil
}

func defaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/images"
	}
	return home + "/.uni/images"
}

func blobsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/blobs"
	}
	return home + "/.uni/blobs"
}

func ociDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".uni/oci"
	}
	return home + "/.uni/oci"
}
