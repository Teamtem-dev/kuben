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
