# flask-mongo

A two-service Jerboa compose stack:

- **db** — MongoDB 4.4 running as a unikernel (from the `eyberg/mongodb` ops package)
- **web** — a Flask app that reads documents from MongoDB and renders them as an
  HTML table

Both services join the `app` bridge network and reach each other by service name
over the daemon's guest DNS: the web app connects to MongoDB at host `db`
(`MONGO_HOST=db` in `stack.yaml`).

## Build the images

```bash
# MongoDB (raw build; mongod initializes an empty /data/db on first start)
jerboa build ../mongodb --name mongodb --pkg eyberg/mongodb:4.4.6 --pkg-source ops

# Flask front-end (python build; requirements.txt is pip-installed into the image)
jerboa build . --name flask-mongo --pkg-source ops --port 8080
```

## Run the stack

```bash
jerboa compose up stack.yaml
jerboa compose ps stack.yaml
```

Then open <http://localhost:8080>. On the first request the app seeds a `people`
collection in the `demo` database if it is empty, then renders it. If MongoDB is
still booting you'll get a "Database not ready" page — just refresh.

Teardown:

```bash
jerboa compose down stack.yaml
```

## Connection details

- **No authentication**: the `eyberg/mongodb` package runs `mongod` wide open, so
  the app connects with no credentials.
- **Ephemeral data**: `mongod` initializes the empty `/data/db` baked into the
  image on first start. For data that survives VM recreation, mount a volume at
  `/data/db` — see `../mongodb/README.md`.

## How it fits together

- `main.py` — Flask app using `pymongo`.
- `requirements.txt` — `flask` + `pymongo`.
- `unikernel.toml` — `lang = "python"`, default memory/port for the web VM.
- `stack.yaml` — the compose definition.
