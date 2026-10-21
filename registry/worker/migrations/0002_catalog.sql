-- What the hub shows about a repository. updated_at is the last tag change,
-- or the creation time of a repository that never had one.
ALTER TABLE repos ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE repos ADD COLUMN readme TEXT NOT NULL DEFAULT '';
ALTER TABLE repos ADD COLUMN pulls INTEGER NOT NULL DEFAULT 0;
ALTER TABLE repos ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE repos SET updated_at = COALESCE((SELECT MAX(updated_at) FROM tags WHERE tags.repo_id = repos.id), created_at);
CREATE INDEX repos_updated ON repos(updated_at);
CREATE INDEX repos_pulls ON repos(pulls);

-- A JSON digest of the recipe for its tag page, so a page view does not read
-- R2. NULL until computed: at push, or on first view for older recipes.
ALTER TABLE recipes ADD COLUMN summary TEXT;
