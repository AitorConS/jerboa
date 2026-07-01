"""Flask app that renders rows from a MySQL unikernel as a simple HTML page.

Connection settings come from the environment so the same image works whether
the database lives on `localhost` or on another VM reached by its compose
service name (see stack.yaml, where MYSQL_HOST=db).

On first request the app lazily creates the database, table, and a few seed
rows if they are missing. That keeps the demo self-contained: the eyberg/mysql
package ships an initialized data directory but no application schema, and
`mysqld --initialize`-style bootstrapping cannot run inside a single-process
unikernel.
"""

import os

import pymysql
from flask import Flask, render_template_string

app = Flask(__name__)

DB_HOST = os.environ.get("MYSQL_HOST", "127.0.0.1")
DB_PORT = int(os.environ.get("MYSQL_PORT", 3306))
DB_USER = os.environ.get("MYSQL_USER", "root")
DB_PASSWORD = os.environ.get("MYSQL_PASSWORD", "root")
DB_NAME = os.environ.get("MYSQL_DATABASE", "demo")

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
    <title>Flask + MySQL on a unikernel</title>
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
    <h1>People served from MySQL</h1>
    <table>
      <thead>
        <tr><th>#</th><th>Name</th><th>Known for</th></tr>
      </thead>
      <tbody>
        {% for person in people %}
        <tr>
          <td>{{ person.id }}</td>
          <td>{{ person.name }}</td>
          <td>{{ person.known_for }}</td>
        </tr>
        {% endfor %}
      </tbody>
    </table>
    <footer>Flask unikernel &rarr; MySQL unikernel at {{ host }}:{{ port }}</footer>
  </body>
</html>
"""


def connect(database=None):
    """Open a connection, optionally to a specific database."""
    return pymysql.connect(
        host=DB_HOST,
        port=DB_PORT,
        user=DB_USER,
        password=DB_PASSWORD,
        database=database,
        cursorclass=pymysql.cursors.DictCursor,
        connect_timeout=5,
    )


def ensure_schema():
    """Create the database, table, and seed rows if they don't exist yet."""
    with connect() as conn:
        with conn.cursor() as cur:
            cur.execute(
                f"CREATE DATABASE IF NOT EXISTS {DB_NAME} "
                "CHARACTER SET utf8mb4"
            )
        conn.commit()

    with connect(DB_NAME) as conn:
        with conn.cursor() as cur:
            cur.execute(
                "CREATE TABLE IF NOT EXISTS people ("
                "  id INT AUTO_INCREMENT PRIMARY KEY,"
                "  name VARCHAR(120) NOT NULL,"
                "  known_for VARCHAR(255) NOT NULL"
                ")"
            )
            cur.execute("SELECT COUNT(*) AS n FROM people")
            if cur.fetchone()["n"] == 0:
                cur.executemany(
                    "INSERT INTO people (name, known_for) VALUES (%s, %s)",
                    SEED_ROWS,
                )
        conn.commit()


def fetch_people():
    with connect(DB_NAME) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT id, name, known_for FROM people ORDER BY id")
            return cur.fetchall()


@app.route("/")
def index():
    try:
        ensure_schema()
        people = fetch_people()
    except pymysql.MySQLError as exc:
        # Most likely the DB VM is still booting; ask the user to retry.
        return (
            f"<h1>Database not ready</h1><p>{exc}</p>"
            f"<p>Retrying MySQL at {DB_HOST}:{DB_PORT} &mdash; refresh in a moment.</p>",
            503,
        )
    return render_template_string(PAGE, people=people, host=DB_HOST, port=DB_PORT)


@app.route("/healthz")
def healthz():
    return "ok\n"


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8080))
    app.run(host="0.0.0.0", port=port)
