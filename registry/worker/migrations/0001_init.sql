-- Accounts are GitHub users; orgs is the JSON list of organizations they
-- belonged to at their last login, which bounds the namespaces they can push.
CREATE TABLE accounts (
  id INTEGER PRIMARY KEY,
  github_id INTEGER NOT NULL UNIQUE,
  login TEXT NOT NULL UNIQUE,
  orgs TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Refresh tokens (login) and personal access tokens (CI), stored as SHA-256.
CREATE TABLE tokens (
  id TEXT PRIMARY KEY,
  account_id INTEGER NOT NULL REFERENCES accounts(id),
  kind TEXT NOT NULL CHECK (kind IN ('refresh', 'pat')),
  name TEXT,
  hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  last_used_at INTEGER,
  revoked_at INTEGER
);
CREATE INDEX tokens_account ON tokens(account_id);

CREATE TABLE repos (
  id INTEGER PRIMARY KEY,
  namespace TEXT NOT NULL,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (namespace, name)
);

-- A pack is charged to the namespace that uploaded it first.
CREATE TABLE packs (
  digest TEXT PRIMARY KEY,
  size INTEGER NOT NULL,
  namespace TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  unused_since INTEGER
);
CREATE INDEX packs_namespace ON packs(namespace);

-- Every blob keeps the location of the first pack that carried it.
CREATE TABLE blobs (
  digest TEXT PRIMARY KEY,
  pack TEXT NOT NULL REFERENCES packs(digest),
  offset INTEGER NOT NULL,
  length INTEGER NOT NULL,
  size INTEGER NOT NULL
);
CREATE INDEX blobs_pack ON blobs(pack);

CREATE TABLE recipes (
  digest TEXT PRIMARY KEY,
  size INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  untagged_since INTEGER
);

-- Which packs a recipe needs; garbage collection counts references per pack.
CREATE TABLE recipe_packs (
  recipe TEXT NOT NULL REFERENCES recipes(digest),
  pack TEXT NOT NULL REFERENCES packs(digest),
  PRIMARY KEY (recipe, pack)
);
CREATE INDEX recipe_packs_pack ON recipe_packs(pack);

CREATE TABLE tags (
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  tag TEXT NOT NULL,
  recipe TEXT NOT NULL REFERENCES recipes(digest),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (repo_id, tag)
);
CREATE INDEX tags_recipe ON tags(recipe);
