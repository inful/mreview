# GitLab webhook setup for `mreview serve`

This walkthrough assumes you already have `mreview serve` running
somewhere reachable by your GitLab instance (e.g.
`http://mreview.internal:8080`).

## One-time

1. Generate a webhook secret:

    ```bash
    openssl rand -hex 32
    ```

    Copy the output — you'll paste it into both GitLab and the
    `mreview serve` startup.

2. Start `mreview serve` with the secret:

    ```bash
    GITLAB_TOKEN=glpat-xxx \
    GITLAB_WEBHOOK_SECRET=<output-of-step-1> \
    mreview serve --addr=:8080
    ```

3. Verify the endpoint is reachable from GitLab's network. The
   easiest check is to open the webhook URL in a browser — you
   should see `405 Method Not Allowed` from `mreview serve` (it only
   accepts POST, and GET is the obvious browser probe). That
   confirms the path is reachable; auth happens at request time.

## Per-project

Repeat these steps in every GitLab project you want reviewed:

1. Open the project → **Settings → Webhooks** (left sidebar).
2. Fill in:
   - **URL**: `http://<your-mreview-host>:8080/webhook`
   - **Secret token**: `<the secret from step 1 above>`
   - **Trigger**: ✅ **Merge request events** only.
     Other events (push, issues, etc.) are ignored by mreview but
     acknowledged with HTTP 204, so leaving them on wastes GitLab
     webhook deliveries.
   - **SSL verification**: enable when the URL is HTTPS.
     Disable only for local development.
3. Click **Add webhook**.
4. In the webhooks list, find the new entry and click **Test** →
   **Merge request events**. The response should be `HTTP 202 Queued`
   (or `204 No Content` if the test payload used an action mreview
   ignores like `close`).
5. In `mreview serve`'s logs you should see:

    ```text
    level=INFO msg="webhook: accepted" project=group/project iid=1 action=open
    ```

That's the full setup. To trigger a real review, open or push a
commit to any MR in the project.

## Instance-wide (optional)

To apply the webhook to every project in a GitLab instance, use
the **Admin → System hooks** page. Note that system hooks fire for
**all** projects; if you want mreview to skip some, add a
per-project allowlist (not currently implemented — see
[issues](https://github.com/inful/mreview/issues)).

## Security notes

- **Use HTTPS in production.** The webhook secret is sent as
  `X-Gitlab-Token` on every delivery — TLS prevents it from leaking
  in transit.
- **Rotate the secret** when operators with access leave the team.
  Rotation is just: pick a new secret, update GitLab's webhook
  setting and the `mreview serve` env var, restart `mreview serve`.
- The HMAC compare is constant-time (`crypto/subtle.ConstantTimeCompare`)
  so timing attacks don't reveal the secret byte-by-byte.
