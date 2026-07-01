"""Flask app that renders rows from a PostgreSQL unikernel as an HTML page.

Mirrors the flask-mysql example but talks to PostgreSQL via pg8000, a pure-Python
driver (no native extensions to compile for the guest). Connection settings come
from the environment; on the compose network the web app reaches PostgreSQL at
host `db` over the daemon's guest DNS.

On first request the app creates the `people` table and a few seed rows if they
are missing. PostgreSQL cannot bootstrap a fresh cluster inside a single-process
unikernel, so the image ships a pre-initialized data directory and the app just
adds its own table to the existing `postgres` database.
"""

import os

import pg8000.dbapi
from flask import Flask, render_template_string

app = Flask(__name__)

DB_HOST = os.environ.get("PGHOST", "127.0.0.1")
DB_PORT = int(os.environ.get("PGPORT", 5432))
DB_USER = os.environ.get("PGUSER", "eyberg")
DB_PASSWORD = os.environ.get("PGPASSWORD", "")
DB_NAME = os.environ.get("PGDATABASE", "postgres")

SEED_ROWS = [
    ("Ada Lovelace", "First published algorithm"),
    ("Alan Turing", "Foundations of computation"),
    ("Grace Hopper", "First compiler"),
    ("Dennis Ritchie", "C and Unix"),
]

PAGE = """
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <title>Flask + PostgreSQL on a unikernel</title>
    <style>
      body { font-family: system-ui, sans-serif; margin: 3rem auto; max-width: 40rem; }
      h1 { font-size: 1.4rem; }
      table { border-collapse: collapse; width: 100%; margin-top: 1rem; }
      th, td { text-align: left; padding: 0.5rem 0.75rem; border-bottom: 1px solid #ddd; }
      th { background: #f4f4f4; }
      footer { margin-top: 2rem; color: #888; font-size: 0.85rem; }
    </style>
  </head>
  <body>
    <h1>People served from PostgreSQL</h1>
    <table>
      <thead>
        <tr><th>#</th><th>Name</th><th>Known for</th></tr>
      </thead>
      <tbody>
        {% for person in people %}
        <tr>
          <td>{{ person[0] }}</td>
          <td>{{ person[1] }}</td>
          <td>{{ person[2] }}</td>
        </tr>
        {% endfor %}
      </tbody>
    </table>
    <footer>Flask unikernel &rarr; PostgreSQL unikernel at {{ host }}:{{ port }}</footer>
  </body>
</html>
"""


def connect():
    return pg8000.dbapi.connect(
        host=DB_HOST,
        port=DB_PORT,
        user=DB_USER,
        password=DB_PASSWORD or None,
        database=DB_NAME,
        timeout=5,
    )


def ensure_schema():
    """Create the table and seed rows if they don't exist yet."""
    conn = connect()
    try:
        cur = conn.cursor()
        cur.execute(
            "CREATE TABLE IF NOT EXISTS people ("
            "  id SERIAL PRIMARY KEY,"
            "  name VARCHAR(120) NOT NULL,"
            "  known_for VARCHAR(255) NOT NULL"
            ")"
        )
        cur.execute("SELECT COUNT(*) FROM people")
        if cur.fetchone()[0] == 0:
            cur.executemany(
                "INSERT INTO people (name, known_for) VALUES (%s, %s)",
                SEED_ROWS,
            )
        conn.commit()
    finally:
        conn.close()


def fetch_people():
    conn = connect()
    try:
        cur = conn.cursor()
        cur.execute("SELECT id, name, known_for FROM people ORDER BY id")
        return cur.fetchall()
    finally:
        conn.close()


@app.route("/")
def index():
    try:
        ensure_schema()
        people = fetch_people()
    except Exception as exc:  # pg8000 raises DatabaseError/InterfaceError subclasses
        return (
            f"<h1>Database not ready</h1><p>{exc}</p>"
            f"<p>Retrying PostgreSQL at {DB_HOST}:{DB_PORT} &mdash; refresh in a moment.</p>",
            503,
        )
    return render_template_string(PAGE, people=people, host=DB_HOST, port=DB_PORT)


@app.route("/healthz")
def healthz():
    return "ok\n"


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8080))
    app.run(host="0.0.0.0", port=port)
