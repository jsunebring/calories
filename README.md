# calorie-log

A self-hosted calorie log, built as a phone web app. Meals are logged by day, with photos. Foods come from:

- barcode scans: [Dabas](https://www.dabas.com) first, then [Open Food Facts](https://world.openfoodfacts.org)
- Livsmedelsverket's food database (synced weekly)
- entries typed by hand
- estimates by Claude from a photo or a description; each one is reviewed before it is logged

One Go binary with SQLite and the page embedded. No build step for the frontend.

## Run

```sh
go run .                       # http://localhost:8080, data in ./data
docker build -t calorie-log . && docker run -p 8080:8080 -v ./data:/data calorie-log
```

The camera needs a secure context, so a phone has to reach the app over HTTPS.

## Configuration

| Variable | |
|---|---|
| `DATA_DIR` | SQLite database and photos. Default `./data`, `/data` in the image. |
| `PORT` | Default `8080`. |
| `ANTHROPIC_API_KEY` | Shared key for estimates, used by anyone who has not saved their own. Optional. |
| `ESTIMATE_MODEL` | Claude model for estimates. |
| `KEY_SECRET` | Encrypts the API keys people save in the app (any long random string). Without it, people cannot save keys. |
| `CF_ACCESS_TEAM`, `CF_ACCESS_AUD` | Cloudflare Access team name and application audience tag. When set, people signed in through Access are identified by their email. |
| `CONTACT_EMAIL` | Sent in the User-Agent to Open Food Facts and Dabas, which ask clients for a way to reach whoever runs them. Set it to yours. |
| `SLV_SYNC` | `off` disables the Livsmedelsverket sync. |

## Users

With Cloudflare Access configured, a user is created on their first visit. Access's allow-list is the list of people.

Without it, users are added from a shell, and a person picks their name and types its email:

```sh
calorie-log user add "Name" name@example.com
calorie-log user list
calorie-log user admin "Name"
```

⚠️ That picker tells people apart. It is not a login. Without Access, put the app on a network you trust.

A request that arrives through Cloudflare (it carries `Cf-Connecting-IP`) is refused unless it has a valid Access token, even when Access is not configured. So if Access stops covering the app, it fails closed instead of offering the picker to the internet.
