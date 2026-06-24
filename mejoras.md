# Arquitectura Windows: daemon en WSL2 (modelo cliente/servidor real)

> Documento de diseño. Objetivo: eliminar la capa de puentes por-operación
> entre Windows y WSL2 y sustituirla por un modelo cliente/servidor limpio,
> donde `unid` corre íntegramente en Linux (WSL2) y `uni.exe` es un cliente
> fino que habla con él por red. La decisión se toma pensando en escalar a
> daemons remotos, no solo en resolver el caso Windows.

---

## 1. Problema actual

Hoy en Windows `unid` corre **nativo** y reenvía operaciones sueltas a WSL2:

- `internal/vm/firecracker_windows.go` — `platformInitFC` reescribe cada hook
  del manager (comando, socket, shutdown, logs, paths) para tunelar a `wsl --`.
- `internal/tools/mkfs_windows.go` — `wslFunc` invoca `mkfs` vía `wsl --` y
  reescribe paths Windows a `/mnt/c/...`.
- `internal/network/*_stub.go`, `internal/vm/cgroup_stub.go` — TAP, bridge,
  cgroups, port-forward **no existen** en Windows: son stubs que devuelven
  error. El daemon nativo de Windows no puede hacer networking ni aislamiento
  reales.

**Consecuencia:** cada capacidad nueva del daemon necesita su propio puente
Windows→WSL2 (reescritura de paths, `curl` al socket interno, quoting de bash).
Es frágil por diseño y no escala.

## 2. Arquitectura objetivo

El daemon **siempre corre en Linux**. Windows deja de ser un caso especial del
daemon y pasa a ser solo un caso especial del *bootstrap* del cliente.

```
Windows                              WSL2 (distro Linux)
──────────────────────────────      ──────────────────────────────────
uni.exe  (cliente fino)   ──TCP──▶   unid (daemon Linux nativo)
  ├── parseo de comandos               ├── QEMU / Firecracker  (nativo)
  ├── resolución de endpoint           ├── TAP / bridge        (nativo)
  ├── bootstrap del daemon             ├── cgroups / KVM       (nativo)
  └── formateo de salida               ├── builder + mkfs      (nativo)
                                       └── image store (ext4 WSL2)
```

En Linux y macOS-con-VM el modelo es idéntico: el cliente habla con un daemon
Linux. Cambia únicamente *dónde* vive el daemon y *cómo* se arranca, no el
protocolo. **Un solo modelo mental para todas las plataformas.**

### Principio rector

> `uni` es multiplataforma y solo contiene lógica de cliente.
> `unid` se compila **solo para Linux** y contiene toda la lógica de
> hipervisor, red y aislamiento.

Esto hace que desaparezcan por construcción los ficheros `*_windows.go` y los
stubs: el daemon nunca se compila para Windows, así que no necesita stubs para
Windows.

---

## 3. Decisiones técnicas

### D1 — Transporte por endpoint con esquema URL (no "TCP a secas")

El protocolo ya es JSON-RPC 2.0 sobre `net.Conn` (`internal/api/client.go`,
`server.go`). En vez de hardcodear TCP, introducimos un **endpoint con esquema**
estilo `DOCKER_HOST`, que es lo que escala a futuro:

```
unix:///var/run/unid.sock      # Linux/macOS local (por defecto)
tcp://127.0.0.1:7890           # WSL2 desde Windows / local TCP
tcps://host:7890               # remoto con TLS (futuro, sin cambiar protocolo)
```

- `api.Dial(endpoint string)` parsea el esquema y elige `net.Dial("unix"|"tcp")`.
- `api.Listen(endpoint string)` simétrico en el servidor
  (`NewServer` deja de recibir `socketPath` y recibe `endpoint`).
- El payload JSON-RPC **no cambia**: streaming de `VM.Attach` incluido funciona
  igual sobre TCP.

> Por qué así y no solo TCP: el día que se quiera un daemon remoto (CI, host de
> build compartido, cluster), solo se añade el esquema `tcps://` y una capa TLS;
> ni el cliente ni los comandos cambian. Es la misma puerta por la que entró
> Docker con `DOCKER_HOST`.

### D2 — Resolución de endpoint y configuración

Prioridad de resolución (de mayor a menor), igual patrón que Docker:

1. Flag `--host` / `-H` (renombra el actual `--socket`, que queda como alias
   deprecado).
2. Variable de entorno `UNI_HOST`.
3. `~/.uni/config.toml`, nueva sección `[daemon]`.
4. Valor por defecto por plataforma.

```toml
# ~/.uni/config.toml
hypervisor = "firecracker"

[daemon]
endpoint = "tcp://127.0.0.1:7890"   # default en Windows
distro   = "jerboa"                  # distro WSL2 a usar (ver D5)
```

Defaults por plataforma (sustituye a `defaultSocketPath()` en
`cmd/uni/main.go:72` y `cmd/unid/main.go:232`):

| Plataforma | Endpoint por defecto         |
|------------|------------------------------|
| Linux/macOS| `unix:///var/run/unid.sock`  |
| Windows    | `tcp://127.0.0.1:7890`       |

### D3 — Seguridad: loopback + token bearer (camino a TLS)

El socket Unix tenía permisos de filesystem; un TCP los pierde. Reglas:

- **Bind a `127.0.0.1`, nunca `0.0.0.0`.** WSL2 hace localhost-forwarding, así
  que `uni.exe` llega por `127.0.0.1` sin exponer el daemon a la LAN.
- **Token bearer obligatorio en cualquier endpoint TCP** (en `unix://` es
  opcional porque ya hay permisos de fichero).
- **El cliente es el dueño del secreto**, no el daemon: `uni.exe` genera un
  token aleatorio en el bootstrap, lo persiste en `%USERPROFILE%\.uni\daemon.json`
  (permisos solo-usuario) y lo pasa al daemon **por stdin/env al lanzarlo**
  (nunca por `argv`, que es visible en la lista de procesos).
- **Handshake de autenticación**: primer frame tras conectar es un método
  `Auth.Hello{token}`. El servidor valida antes de despachar cualquier otra
  cosa. Encaja en el JSON-RPC actual sin tocar el resto de llamadas.

> Esto deja la puerta abierta a `tcps://` + mTLS para remoto sin rediseñar:
> el handshake pasa de "token" a "certificado" y el resto del protocolo no se
> entera.

### D4 — El build se mueve dentro del daemon (adiós reescritura de paths)

Hoy `uni build` compila en el host y luego invoca `mkfs` (vía WSL en Windows,
reescribiendo paths). Esto es la otra mitad de la fragilidad. Decisión:

- El daemon expone `Image.Build`. El cliente **empaqueta el contexto de build**
  (binario ya compilado o fuente + manifiesto) y lo **streamea** al daemon.
- compile (si aplica) + `mkfs` ocurren **dentro de Linux**, contra el store de
  WSL2. Cero reescritura de paths `/mnt/c/...`, cero quoting de bash.
- **Beneficio transversal:** el build pasa a ser idéntico en toda plataforma —
  siempre en Linux — lo que elimina divergencias de comportamiento Windows vs
  Linux y es el modelo que usa BuildKit (build remoto/compartible) si algún día
  se quiere.

`internal/tools/mkfs.go` deja de tener rama `runtime.GOOS == "windows"`:
solo existe `directFunc`, porque siempre se ejecuta en Linux.

### D5 — Image store en ext4 de WSL2, nunca en `/mnt/c`

El store del daemon vive en el filesystem ext4 de la distro WSL2
(p.ej. `~/.uni/images` *dentro* de la distro), **no** en `/mnt/c/...` (9p es
lento). El cliente no accede al store por filesystem: lo consulta por RPC
(`Image.List`, etc.) y transfiere bytes por la conexión. Frontera limpia y
única: todo lo que cruza Windows↔WSL2 va por el endpoint, nada por paths
compartidos.

### D6 — Gestión del daemon y de la distro (modelo Docker Desktop)

`uni.exe` gestiona el ciclo de vida como hace `docker.exe`:

1. **Health check**: ¿responde el endpoint? Si sí, conecta.
2. **Auto-arranque**: si no, lanza `wsl -d <distro> -- unid --host tcp://127.0.0.1:7890 ...`
   con el token por stdin, espera a que el health check pase.
3. **Distro dedicada (estado final, recomendado):** jerboa aprovisiona su propia
   distro vía `wsl --import jerboa <ruta> <rootfs.tar>`, igual que Docker usa
   `docker-desktop`. Ventaja: entorno reproducible (kernel con KVM, binarios,
   versiones) sin depender de lo que el usuario tenga instalado.
   - **Interino aceptable:** usar la distro por defecto del usuario validando
     prerequisitos (`/dev/kvm`, virtualización anidada para Firecracker) y
     fallando con un mensaje accionable si faltan.

> La distro dedicada es la decisión "limpia y escalable": versionas el rootfs
> junto al release, garantizas KVM/nested-virt, y desinstalar jerboa es borrar
> la distro. Se deja para una fase posterior por coste, pero es el objetivo.

### D7 — Separación de módulos: cliente multiplataforma, daemon solo-Linux

Reorganización para que la separación sea estructural, no por `if GOOS`:

- `uni` (cliente): depende de `internal/api` (cliente), `internal/config`,
  formato de salida y `internal/wslboot` (bootstrap, build-tag `windows`).
  No importa `internal/vm`, `internal/network`, etc.
- `unid` (daemon): concentra `internal/vm`, `internal/network`,
  `internal/builder`, hipervisores vía `internal/apiserver`.

**Estado:**

- **D7-A ✅** — `internal/api` dividido en cliente (`api`) y servidor
  (`apiserver`). Verificado: `go list -deps ./cmd/uni` ya no enlaza
  `internal/vm` ni `internal/network`. El parseo de puertos/endpoint y el
  framing viven en `api` para que la CLI no importe `vm`.
- **D7-C ✅** — `uni pkg load` enruta por el daemon (`Image.Build`); `mkfs.go`
  sin rama Windows; `mkfs_windows.go`/`mkfs_unix.go` (+ tests) borrados;
  `firecracker_windows.go`/`_notwindows.go` unificados en `platformInitFC`
  no-op universal.
- **D7-B ✅** — Daemon solo-Linux por restricción de build. Borrados los stubs
  (`tap_stub.go`, `bridge_stub.go`, `portfwd_stub.go`, `cgroup_stub.go`,
  `stats_stub.go`). Marcadas `//go:build linux` todas las fuentes del lado
  daemon: `internal/{vm,network,apiserver,metrics,scheduler,service,tracing,ui}`,
  `cmd/unid`, `cmd/uni-smoke`, y los tests de `cmd/uni` que arrancan el daemon
  in-process. El cliente (`uni`) y sus paquetes portables compilan y testean en
  cualquier OS; el lado daemon compila/testea solo en Linux (validado con
  `GOOS=windows` y `GOOS=linux` build+vet). El test de salud de `wslboot` que
  necesita un daemon real vive en `health_linux_test.go`.

  *Coste asumido a propósito:* los tests de la CLI que dependen del daemon
  in-process (≈10 ficheros) y todo el lado servidor ya no se ejecutan en
  Windows; el desarrollo del daemon se hace en WSL2/Linux.

---

## 4. Plan de migración por fases

Cada fase es entregable y deja el sistema funcionando.
**Estado: Fases 1-3 completas y validadas end-to-end en WSL2 real.**

### Fase 1 — Transporte por endpoint (barata, alto valor) ✅
- [x] `api.Dial(endpoint)`/`api.listen(endpoint)` con parseo de esquema
      (`unix://`, `tcp://`; valor desnudo = unix por compatibilidad).
- [x] `unid` escucha en el endpoint resuelto; `--host` con alias `--socket`.
- [x] `uni` resuelve endpoint (flag → `UNI_HOST` → config → default plataforma).
- [x] Validación end-to-end por TCP loopback Windows↔WSL2.
- [x] **Borrado** `firecracker_windows.go` (`platformInitFC` ahora no-op universal).

### Fase 2 — Build y store dentro del daemon ✅
- [x] RPC `Image.Build` con streaming del contexto (frames length-prefixed).
- [x] Store del daemon en ext4 de WSL2; cliente por RPC (`Image.List/Remove`,
      run/build por ref `name:tag`). mkfs corre en Linux (resolución lazy).
- [x] `tools/mkfs.go` sin rama Windows; `mkfs_windows.go` borrado (D7-C).
      `uni pkg load` ya enruta su build por el daemon.

### Fase 3 — Seguridad y bootstrap ✅
- [x] Token bearer + handshake `Auth.Hello` (comparación constante); bind
      loopback; aviso si TCP sin token.
- [x] `internal/wslboot`: health check + auto-arranque de `unid` en WSL2,
      token por entorno (`WSLENV`, nunca `argv`), persistencia en
      `%USERPROFILE%\.uni\daemon.json` (0600).

### Fase 4 — Distro dedicada y limpieza final (parcial)
- [ ] Aprovisionamiento de distro `jerboa` vía `wsl --import` (rootfs versionado).
- [x] Separación de módulos D7 consolidada (D7-A/B/C). Cliente multiplataforma;
      daemon `//go:build linux`. Stubs de red/cgroup **borrados** (ver D7-B).

### Firma de imágenes — re-cableada al store del daemon ✅
- [x] `uni sign`/`uni verify` resuelven el *disk digest* de la imagen vía
      `Image.Get` (RPC nuevo) y firman/verifican por digest, no por directorio
      local. Las firmas se guardan en `<root>/signatures/<digest>.sig`.
- [x] `signing.Store`: `SignDigest`/`VerifyDigest` (Ed25519, clave por defecto
      generada en primer uso). `VerifyDigest` rechaza una firma cuyo digest no
      coincide con el solicitado.
- [x] `uni run --verify` valida contra el store del daemon; los runs por ruta de
      fichero (sin `name:tag`) no tienen referencia que verificar.

---

## 5. Qué NO cambia

- El protocolo JSON-RPC y todos los métodos existentes (`VM.*`, `Network.*`,
  `Service.*`, `DNS.*`). Solo se añade `Auth.Hello` e `Image.Build`.
- La experiencia en Linux/macOS: sigue siendo `unix://` por defecto.
- El modelo de comandos del CLI.

## 6. Riesgos y mitigaciones

| Riesgo | Mitigación |
|--------|------------|
| Localhost-forwarding de WSL2 falla (modo *mirrored* en WSL nuevo) | Health check con timeout y mensaje accionable. **Validado**: en modo NAT el relay de WSL sirve un servicio WSL bound a `127.0.0.1` a Windows sin problemas |
| `/dev/kvm` o virtualización anidada no disponibles | Validación en bootstrap; Firecracker degrada a QEMU si falta KVM |
| Token filtrado | Solo loopback + permisos solo-usuario en `daemon.json`; nunca en `argv` |
| Coste de la distro dedicada | Fase 4 opcional; interino usa distro existente del usuario |
