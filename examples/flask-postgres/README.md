# flask-postgres

A two-service Jerboa compose stack:

- **db** — PostgreSQL 11 running as a unikernel (from the `eyberg/postgresql` ops package)
- **web** — a Flask app that reads rows from PostgreSQL and renders them as an
  HTML table

Both services join the `app` bridge network and reach each other by service name
over the daemon's guest DNS: the web app connects to PostgreSQL at host `db`
(`PGHOST=db` in `stack.yaml`).

## Build the images

```bash
# PostgreSQL (raw build; bakes an open pg_hba.conf — see examples/postgresql)
jerboa build ../postgresql --name postgresql --pkg eyberg/postgresql:11.3.0 --pkg-source ops

# Flask front-end (python build; requirements.txt is pip-installed into the image)
jerboa build . --name flask-postgres --pkg-source ops --port 8080
```

## Run the stack

```bash
jerboa compose up stack.yaml
jerboa compose ps stack.yaml
```

Then open <http://localhost:8080>. On the first request the app creates a
`people` table in the `postgres` database and seeds a few rows, then renders
them. If PostgreSQL is still booting you'll get a "Database not ready" page —
just refresh.

Teardown:

```bash
jerboa compose down stack.yaml
```

## Connection details

- **User `eyberg`**: the `eyberg/postgresql` package's pre-initialized cluster
  was created by the `eyberg` OS user, so that is the bootstrap superuser
  (`PGUSER=eyberg`). Trust authentication means no password.
- **Open `pg_hba.conf`**: the package's baked `pg_hba` only trusts localhost and
  the old SLIRP host, so `examples/postgresql` copies an open `pg_hba.conf` into
  the image and points postgres at it with `-c hba_file=/pg_hba.conf`. Without
  this, a client on the bridge network is rejected.

## Restarting postgres

The unikernel's root filesystem is ephemeral: guest writes are discarded when
the VM stops, so each boot starts from the pristine baked `/db`. That means
`compose up` / `down` cycles work repeatedly with no stale `postmaster.pid` or
lock files to clean up.

Because `/db` is ephemeral, rows created at runtime do not survive a restart
(the app re-seeds them on first request). For durable data across recreation,
mount a seeded volume at `/db` — see `../postgresql/README.md`.

## How it fits together

- `main.py` — Flask app using `pg8000` (a pure-Python driver, no native
  extensions to compile for the guest).
- `requirements.txt` — `flask` + `pg8000`.
- `unikernel.toml` — `lang = "python"`, default memory/port for the web VM.
- `stack.yaml` — the compose definition.
