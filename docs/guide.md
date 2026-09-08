# Kuben user guide: ten everyday scenarios

Every scenario works from the web UI and from the REST API (`/api/docs` serves the full OpenAPI reference). The examples assume:

```bash
export KUBEN_URL=https://kuben.example.com
export KUBEN_TOKEN=kbn_pat_…        # scenario 3
```

and a project `shop` with the environments `staging` and `production`.

---

## 1. Sign in safely

Kuben throttles failed logins with three counters in a 15-minute window:

| Counter | Default limit | Stops |
|---|---|---|
| email + client IP | 5 | guessing one account from one place |
| client IP | 30 | one client trying many accounts |
| email | 100 | a botnet guessing one account |

A blocked attempt gets `429 Too Many Requests` with a `Retry-After` header. A correct password clears only the email + IP counter.

**Behind a proxy.** Set `KUBEN_SECURITY__TRUST_FORWARDED_FOR=true` only when a proxy appends the client address (the Helm chart does this for the Gateway). Kuben then uses the **last** `X-Forwarded-For` hop; the first hops are written by the client and are ignored.

Tune the limits with `KUBEN_SECURITY__LOGIN_MAX_FAILURES`, `…_PER_IP`, `…_PER_ACCOUNT` and `KUBEN_SECURITY__LOGIN_WINDOW_SECS`.

## 2. See who changed what (audit log)

Every create, update, delete, restart, rollback, promotion, invitation and token change is recorded automatically. So is every denied or failed attempt, and every login. Each record holds:

- the actor (user or token)
- the action (the OpenAPI operation id, e.g. `createApp`)
- the target (`shop/prod/api`)
- the outcome (`success`, `denied`, `failure`, `throttled`, `error`) and HTTP status
- the client IP and request id

Request bodies, and therefore secret values, are never recorded.

- **UI:** *Audit* in the top bar (owners and admins).
- **API:**

```bash
curl -fsS "$KUBEN_URL/api/v1/audit?limit=100" -H "Authorization: Bearer $KUBEN_TOKEN"
# older pages: …/audit?before=<next_before of the previous page>
```

## 3. Deploy from CI with an API token

1. Open *API tokens* and create a token:
   - **Role:** `developer` (it can deploy but not administer)
   - **Project:** `shop`
   - **Environment:** `staging`
   - **Lifetime:** 90 days by default, 365 at most
2. Copy it (it is shown once) and store it as the repository secret `KUBEN_TOKEN`.

A token never has more rights than its owner. It cannot create tokens, manage members or change passwords, even if it leaks.

**GitHub Actions** — build, push, then roll out the new image:

```yaml
name: deploy
on: { push: { branches: [main] } }
permissions: { contents: read, packages: write }
jobs:
  deploy:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v5
      - uses: docker/login-action@v3
        with: { registry: ghcr.io, username: "${{ github.actor }}", password: "${{ secrets.GITHUB_TOKEN }}" }
      - uses: docker/build-push-action@v6
        with: { push: true, tags: "ghcr.io/${{ github.repository }}:${{ github.sha }}" }
      - name: Roll out on Kuben
        run: |
          curl -fsS -X PATCH "${{ vars.KUBEN_URL }}/api/v1/projects/shop/environments/staging/apps/api" \
            -H "Authorization: Bearer ${{ secrets.KUBEN_TOKEN }}" \
            -H 'Content-Type: application/json' \
            -d '{"image": "ghcr.io/${{ github.repository }}:${{ github.sha }}"}'
```

Revoke a token at any time from the same page (`DELETE /api/v1/tokens/{id}`); it stops working immediately.

## 4. Work as a team

Roles form a strict ladder, each including everything below it:

| Role | Can |
|---|---|
| viewer | read projects, apps and logs |
| developer | + deploy, change apps, open terminals, read secrets |
| admin | + create projects/environments, write secrets, promote, read the audit log, invite members |
| owner | + manage owners and delete protected environments |

- **Invite.** On *Team*, enter an email and a role. A new account gets a **temporary password shown once**. Share it over a secure channel.
- **First sign-in.** The invitee must replace the temporary password first. The API answers `403` to everything else until they do.
- **The rules:**
  - nobody grants a role above their own
  - only owners change or remove owners
  - the last owner can be neither demoted nor removed
- **Removing a member** revokes their sessions and API tokens immediately.
- **Changing your own password** (*Account*) signs out your other sessions.

## 5. Roll back a bad release

Every change to an app is recorded as a numbered revision. That covers deploys, configuration changes, rollbacks, promotions and template deploys.

- **UI:** the *Releases* card on the app page. Press *Roll back* next to any earlier revision.
- **API:**

```bash
APP="$KUBEN_URL/api/v1/projects/shop/environments/production/apps/api"
curl -fsS "$APP/releases" -H "Authorization: Bearer $KUBEN_TOKEN"
curl -fsS -X POST "$APP/rollback" -H "Authorization: Bearer $KUBEN_TOKEN" \
  -H 'Content-Type: application/json' -d '{"revision": 41}'
```

**What a rollback restores and keeps.** It restores the image, processes and environment variables of that revision. It keeps today's domains and volumes, because data never moves backwards. The rollback itself becomes a new revision.

**Safe rollouts:**
- New pods must pass their readiness check before old ones stop.
- Slow starters get up to 5 minutes (startup probe).
- A rollout that makes no progress for 10 minutes is reported as `RolloutFailed`.

## 6. Keep data on a volume

Add a volume when deploying: *Volume* `/data:5Gi`, or in the API:

```json
{ "name": "wiki", "image": "…", "port": 3000,
  "volumes": [{ "name": "data", "mount_path": "/data", "size": "5Gi" }] }
```

- **One pod.** A volume is `ReadWriteOnce`, so the app runs a single pod, and deploys stop the old pod before starting the new one (a few seconds of downtime).
