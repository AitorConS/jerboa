# flask-mysql

A two-service Jerboa compose stack:

- **db** — MySQL 5.7 running as a unikernel (from the `eyberg/mysql` ops package)
- **web** — a Flask app that reads rows from MySQL and renders them as a simple
  HTML table

Both services join the `app` bridge network and reach each other by service
name over the daemon's guest DNS: the web app connects to MySQL at host `db`
(`MYSQL_HOST=db` in `stack.yaml`).

## Build the images

The compose file references two images by name, so build both first.

MySQL (uses the `unikernel.toml` from the `mysql` example, `raw` build):

```bash
jerboa build ../mysql --name mysql --pkg eyberg/mysql:5.7.29 --pkg-source ops
```

Flask front-end (python build; `requirements.txt` is pip-installed into the
image, `main.py` is the entrypoint):

```bash
jerboa build . --name flask-mysql --pkg-source ops --port 8080
```

## Run the stack

```bash
jerboa compose up stack.yaml
jerboa compose ps stack.yaml
```

Then open <http://localhost:8080>. On the first request the app creates the
`demo` database, a `people` table, and a few seed rows if they don't exist yet,
then renders them. If MySQL is still booting you'll get a "Database not ready"
page — just refresh.

The app authenticates as `root`/`root`: the `eyberg/mysql` package ships a
wide-open `root` account with password `root` (see its README). Override with
the `MYSQL_USER` / `MYSQL_PASSWORD` env vars in `stack.yaml` for a different
image.

Logs and teardown:

```bash
jerboa compose logs stack.yaml web
jerboa compose logs stack.yaml db
jerboa compose down stack.yaml
```

## How it fits together

- `main.py` — Flask app; connection settings are read from `MYSQL_*` env vars
  and rendered via `render_template_string`.
- `requirements.txt` — `flask` + `pymysql` (a pure-Python MySQL driver, so no
  native extensions need to compile for the guest).
- `unikernel.toml` — `lang = "python"`, default memory/port for the web VM.
- `stack.yaml` — the compose definition (services, network, env, ports).

## Persistence (optional)

By default MySQL data is ephemeral: `mysqld` uses the pre-initialized
`/var/lib/mysql` baked into the image, and the `demo` database is recreated by
the web app whenever it's missing.

To keep data across VM recreation, seed a volume with the initialized data
directory once, then mount it on the `db` service:

```bash
jerboa volume create mysqldata --size 512M
jerboa volume seed   mysqldata --pkg eyberg/mysql:5.7.29 --pkg-source ops --src /var/lib/mysql
```

Then add to the `db` service in `stack.yaml`:

```yaml
    volumes:
      - mysqldata:/var/lib/mysql
```

and declare the volume at the top level:

```yaml
volumes:
  mysqldata:
    size: 512M
```

## Notes

- Services resolve each other by name through the daemon's guest DNS server, so
  `MYSQL_HOST=db` just works — no static IPs needed. To pin an address anyway,
  set `ip:` on a service in `stack.yaml`.
- Port publishing requires the managed network, which `stack.yaml` declares as
  `app`.
