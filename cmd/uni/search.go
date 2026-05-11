package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func newSearchCmd(regCfg *registryClientConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "search <registry>/<query>",
		Short: "Search registry repositories by substring",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			registryURL, query, err := parseRegistryQuery(args[0])
			if err != nil {
				return fmt.Errorf("search: %w", err)
			}
			client, err := newRegistryClient(registryURL, regCfg)
			if err != nil {
				return fmt.Errorf("search: %w", err)
			}
			repos, err := client.ListRepositories(cmd.Context())
			if err != nil {
				return fmt.Errorf("search: %w", err)
			}
			query = strings.ToLower(query)
			matches := make([]string, 0)
			for _, repo := range repos {
				if strings.Contains(strings.ToLower(repo), query) {
					matches = append(matches, repo)
				}
			}
			sort.Strings(matches)
			for _, m := range matches {
				fmt.Fprintln(cmd.OutOrStdout(), m)
			}
			return nil
		},
	}
}

func parseRegistryQuery(arg string) (string, string, error) {
	idx := strings.LastIndex(arg, "/")
	if idx <= 0 || idx >= len(arg)-1 {
		return "", "", fmt.Errorf("expected <registry>/<query>")
	}
	registryURL := arg[:idx]
	query := arg[idx+1:]
	if !strings.HasPrefix(registryURL, "http://") && !strings.HasPrefix(registryURL, "https://") {
		registryURL = "http://" + registryURL
	}
	return registryURL, query, nil
}
