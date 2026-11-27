# Migrating a Multica Cloud workspace to a self-hosted server

This guide moves one workspace — issues, comments, chats, agents, skills, autopilots, attachments and avatars — from Multica Cloud (or any Multica server) to your own deployment, for example a Docker Compose or Coolify install. No hand-written SQL is needed.

The migration has three stages:

1. **Export** the workspace from the cloud with the CLI (`multica workspace export`).
2. **Build** a restore package on your machine: `build_selfhost_dump.sh` merges the archive into a snapshot of your self-host database, then packages a PostgreSQL dump and an uploads tarball.
3. **Restore** both on the server, adjust the environment, and reconnect clients.

Examples below use `https://multica.example.com` for the web app and `https://api.multica.example.com` for the API. Replace them with your own hosts.

## Prerequisites

- **A running self-host on the same version as this checkout.** The importer writes the schema of the checkout it was built from: deploy the self-host from the same release (or a newer one), and let it run its migrations once before you take the snapshot.
- **A personal access token of the cloud workspace owner** (`mul_…`, created under *Settings → API Token*). An agent's task token (`mat_…`) cannot read the whole workspace, and a member token misses settings only owners and admins see.
- **The CLI from this checkout**, which includes `multica workspace export` (`cd server && go build -o multica ./cmd/multica`).
- **PostgreSQL 17 client tools** (`psql`, `pg_dump`, `pg_restore`) matching the server, *or* Docker (the build script then starts a throwaway `pgvector/pgvector:pg17` container).
- **Go** to build the importer (the script does it for you), or a prebuilt `import-workspace` binary passed with `IMPORTER=`.
- **Local file storage** on the self-host (`LOCAL_UPLOAD_DIR`). For S3 storage, run the importer yourself with the `S3_*` variables of the self-host instead of extracting the tarball.

## 1. Export the workspace

Point the CLI at the cloud with the owner's token and export:

```bash
export MULTICA_SERVER_URL=https://api.multica.ai
export MULTICA_TOKEN=mul_xxxxxxxx            # the owner's personal access token
export MULTICA_WORKSPACE_ID=<workspace-uuid>  # or select the workspace with the CLI
./multica workspace export --out ./workspace-export
```

The export is resumable: re-run the same command after an interruption. It finishes by writing `manifest.json`; the importer refuses an archive without it.

| Flag | Effect |
| --- | --- |
| `--skip-transcripts` | Leave out agent run messages (smaller archive) |
| `--skip-attachments` | Leave out attachment files (rows are still exported) |
| `--include-secrets` | Export agent custom environment values. Off by default; keep the archive private when you use it |

The archive contains workspace data in clear text. Store it on an encrypted disk and delete it once the migration is verified.

## 2. Snapshot the self-host database

The import runs against a copy of your current self-host database, so it can tell which accounts already exist there (someone who already signed in, for example). Stop the backend so nothing writes during the snapshot, then dump the database in custom format:

```bash
# Coolify: open the terminal of the PostgreSQL container, or run from the host:
docker exec <postgres-container> pg_dump -U multica -d multica -Fc > selfhost-before.dump
```

Keep `selfhost-before.dump`: it is also your rollback.

## 3. Build the restore package

From the root of this checkout:

```bash
WEB_URL=https://multica.example.com \
server/cmd/import-workspace/scripts/build_selfhost_dump.sh \
  ./workspace-export ./selfhost-before.dump ./selfhost-build
```

The script:

1. restores the snapshot into a temporary database (`multica_selfhost_build`);
2. imports the archive with its IDs, authors and dates. Attachment and avatar files go to a local uploads directory, and links in content are rewritten to relative `/api/attachments/<id>/download` routes. Issue links to the source app (`SOURCE_APP_URL`, default `https://multica.ai`) are rewritten to `WEB_URL` when it is set;
3. checks that every file the importer reported is present;
4. writes `selfhost-build/multica-selfhost.dump` and `selfhost-build/multica-uploads.tgz`.

Nothing is packaged when the import fails.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PG_MODE` | `docker` when Docker is available and `PG_ADMIN_URL` is unset, else `local` | Where the temporary database runs |
| `PG_ADMIN_URL` | — | `local` mode: URL of a PostgreSQL 17 role allowed to create databases, e.g. `postgres://postgres@localhost:5432/postgres` |
| `PG_BIN` | `PATH` | `local` mode: directory holding the PostgreSQL 17 client tools |
| `PG_IMAGE` | `pgvector/pgvector:pg17` | `docker` mode image |
| `IMPORTER` | built from the checkout | Path to a prebuilt `import-workspace` binary |
| `SOURCE_APP_URL` | `https://multica.ai` | Web URL of the source server |
| `WEB_URL` | — | Web URL of the self-host |

Any extra argument after the three paths is passed to the importer.

### Accounts that already exist on the self-host

If an archived member's email already belongs to another account on the self-host, the import stops and lists the conflicts:

```text
1 archived account(s) share an email with a different account in the target:
  bob@example.com: archive 6100d5d1-…, target 239e5bec-…
```

Re-run the script with one of:

- `-merge-existing-users`: attach the imported history (issues, comments, memberships) to the existing self-host account;
- `-email-map bob@example.com=bob@corp.example.com` (repeatable): import the archived account under another address.

## 4. Restore on the server

Copy both files to the server, stop the backend, then restore. Pick whichever fits your setup.

From the PostgreSQL container (Coolify: *Terminal* tab of the database resource, after uploading the dump into the container, or with `docker exec -i` from the host):

```bash
docker exec -i <postgres-container> pg_restore -U multica -d multica \
  --clean --if-exists --no-owner --no-acl < multica-selfhost.dump
```

From the backend container (it needs PostgreSQL 17 tools; on Alpine `apk add postgresql17-client`):

```bash
pg_restore -d "$DATABASE_URL" --clean --if-exists --no-owner --no-acl multica-selfhost.dump
```

Then extract the files into the backend's upload directory (the persistent volume behind `LOCAL_UPLOAD_DIR`) and check that none is missing:

```bash
tar -xzf multica-uploads.tgz -C "$LOCAL_UPLOAD_DIR"
tar -tzf multica-uploads.tgz | grep -v '/$' | while read -r f; do
  [ -f "$LOCAL_UPLOAD_DIR/$f" ] || echo "missing: $f"
done
```

Start the backend again.

## 5. Environment checklist

Check these variables on the backend before you sign in. Redeploy after any change: they are read at startup.

| Variable | Value |
| --- | --- |
| `MULTICA_APP_URL` | `https://multica.example.com`, scheme included |
| `MULTICA_PUBLIC_URL` | `https://api.multica.example.com`, scheme included |
| `COOKIE_DOMAIN` | Required when the app and the API are on two hosts and the browser calls the API directly: the narrowest parent domain covering both, e.g. `.multica.example.com`. The session cookie is then sent to every host under that domain, so only use a domain whose hosts you all trust. Leave it empty for a single-domain setup |
| `CORS_ALLOWED_ORIGINS` | The web app origin, e.g. `https://multica.example.com`, when it differs from the API origin |
| `LOCAL_UPLOAD_DIR` | A persistent volume; the directory you extracted the tarball into |
| `JWT_SECRET` | Keep it stable: avatar links are signed with it |

See [Environment variables](../apps/docs/content/docs/environment-variables.mdx) for the full list.

## 6. After the migration

1. **Sign in again.** Clear the cookies of the app and API hosts (and of the parent domain if you set `COOKIE_DOMAIN`), then sign in with email.
2. **Account emails.** Accounts keep the email they had in the cloud. To sign in with another address, rebuild from the same snapshot with `-email-map old@example.com=new@example.com` and restore again; Multica has no email change screen.
3. **CLI.** Create a separate profile so the cloud profile keeps working while its daemon is still running:

   ```bash
   multica --profile selfhost setup self-host \
     --server-url https://api.multica.example.com \
     --app-url https://multica.example.com
   ```

   Do not re-run `multica setup` on the default profile while a cloud daemon uses it.
4. **Desktop.** Create `~/.multica/desktop.json`:

   ```json
   { "schemaVersion": 1, "apiUrl": "https://api.multica.example.com" }
   ```

   Desktop derives the web URL by removing a leading `api.` from the API host. Add `"appUrl": "https://multica.example.com"` when your API host does not start with `api.`.
5. **Runtimes.** Start a daemon against the self-host (`multica --profile selfhost daemon start`). Then open each agent and check that its runtime is online; select the new one where needed.
6. **MCP servers.** They are not imported (they hold credentials). The importer report lists each agent to reconfigure (`agent "…": re-attach N MCP server(s)`).
7. **Cloud autopilots.** Pause them in the cloud workspace so scheduled runs do not fire twice.
8. **Verify.** Open a few issues with images, the workspace and agent avatars, and a chat. Then delete the export archive and the build directory.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| Avatars return 404 | The files are not under `LOCAL_UPLOAD_DIR`, or the backend uses another directory. Re-extract the tarball into the directory the backend actually uses and run the check above |
| Images in issues are broken (401) | Links still point at the old server. Packages built with this script use relative `/api/attachments/<id>/download` links; rebuild if you used an older importer. Also check that you are signed in on the API host (`COOKIE_DOMAIN`) |
| "invalid or expired code" when signing in | The restore replaced the `verification_code` table, and codes requested before the restore are marked used or are gone. Request a new code. Check with `SELECT used, expires_at FROM verification_code WHERE email = 'you@example.com' ORDER BY created_at DESC LIMIT 1` |
| "CSRF validation failed" on every write | The browser cannot read the CSRF cookie across hosts. Set `COOKIE_DOMAIN`, redeploy, clear cookies, sign in again |
| `duplicate key value violates unique constraint "user_email_key"` | The snapshot used for the build is older than the target database. Take a fresh snapshot with the backend stopped and rebuild; resolve conflicts with `-merge-existing-users` or `-email-map` |
| `openssl: not found` in a container | Alpine images ship without it: `apk add openssl` |
| `file count mismatch: do not restore this build` | Some files could not be written to the build's uploads directory. Check disk space and permissions, and start again with a fresh output directory |

## Rollback

Stop the backend, restore `selfhost-before.dump` with `pg_restore --clean --if-exists`, and remove the extracted files (the tarball lists them).
