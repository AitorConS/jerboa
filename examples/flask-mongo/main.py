"""Flask app that renders documents from a MongoDB unikernel as an HTML page.

Mirrors the flask-mysql / flask-postgres examples but talks to MongoDB via
pymongo. Connection settings come from the environment; on the compose network
the web app reaches MongoDB at host `db` over the daemon's guest DNS.

On first request the app seeds a `people` collection if it is empty, then
renders it. mongod initializes an empty data directory on first start, so no
pre-seeding of the database is needed.
"""

import os

from flask import Flask, render_template_string
from pymongo import MongoClient

app = Flask(__name__)

DB_HOST = os.environ.get("MONGO_HOST", "127.0.0.1")
DB_PORT = int(os.environ.get("MONGO_PORT", 27017))
DB_NAME = os.environ.get("MONGO_DATABASE", "demo")

SEED_ROWS = [
    {"id": 1, "name": "Ada Lovelace", "known_for": "First published algorithm"},
    {"id": 2, "name": "Alan Turing", "known_for": "Foundations of computation"},
    {"id": 3, "name": "Grace Hopper", "known_for": "First compiler"},
    {"id": 4, "name": "Dennis Ritchie", "known_for": "C and Unix"},
]

PAGE = """
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <title>Flask + MongoDB on a unikernel</title>
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
    <h1>People served from MongoDB</h1>
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
    <footer>Flask unikernel &rarr; MongoDB unikernel at {{ host }}:{{ port }}</footer>
  </body>
</html>
"""


def get_collection():
    client = MongoClient(
        host=DB_HOST,
        port=DB_PORT,
        serverSelectionTimeoutMS=5000,
    )
    return client[DB_NAME]["people"]


def ensure_seed(collection):
    if collection.estimated_document_count() == 0:
        collection.insert_many([dict(row) for row in SEED_ROWS])


@app.route("/")
def index():
    try:
        collection = get_collection()
        ensure_seed(collection)
        people = list(collection.find({}, {"_id": 0}).sort("id"))
    except Exception as exc:  # pymongo raises ServerSelectionTimeoutError etc.
        return (
            f"<h1>Database not ready</h1><p>{exc}</p>"
            f"<p>Retrying MongoDB at {DB_HOST}:{DB_PORT} &mdash; refresh in a moment.</p>",
            503,
        )
    return render_template_string(PAGE, people=people, host=DB_HOST, port=DB_PORT)


@app.route("/healthz")
def healthz():
    return "ok\n"


if __name__ == "__main__":
    port = int(os.environ.get("PORT", 8080))
    app.run(host="0.0.0.0", port=port)
