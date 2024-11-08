# 🥔 Potato Analytics

A lightweight and minimalistic analytics solution that tracks your visitors' countries, sources, and page views while respecting their privacy. Ideal for personal websites or blogs. Potato Analytics only tracks aggregate data and stores nothing about individual visitors. It makes it light, fast, and minimizes maintenance.

Other key features are:

- Lightweight (< 2KB compressed)
- Privacy friendly (doesn't store any personal information)
- Uses caching to minimize unnecessary requests
- Automatically filters out bots and crawlers
- Accessible via a simple API
- Uses a probabilistic data structure called [HyperLogLog](https://en.wikipedia.org/wiki/HyperLogLog) to estimate the number of unique visitors which saves on storage

For more details, see the associated [blog post](https://www.integralreview.com/potato-analytics).

## Usage

### Deployment

The easiest way to deploy Potato Analytics is to use the Docker image:

Here are the necessary environment variables:

- `DATABASE_URL`: The URL of your PostgreSQL database (e.g. `postgres://postgres:your-password@your-host:your-port/your-database`).
- `DOMAIN`: The tracking domain of your website (e.g. `analytics.your-website.com`).
- `API_KEY`: A secret key to authenticate your requests.
- `ENVIRONMENT`: The environment (e.g. `development` or `production`).

You'll also need to set up a PostgreSQL database with the HLL extension available. Here's a built docker image with it available: [https://github.com/antoinefink/docker-postgres-hll](https://github.com/antoinefink/docker-postgres-hll). If you do not want to bother setting up PostgreSQL, you should be able to get away with the free tier of [Supabase](https://supabase.com/) although there's always the risk that one day they will downgrade their free tier.

### Installation
Add the following script to your website's HTML (ideally just before the closing `</body>` tag):

```html
<script src="https://your-analytics-domain.com/analytics.js" defer></script>
```

### Obtaining your stats
To check your stats, use the `/stats/pages`, `/stats/countries`, and `/stats/sources` endpoints:

```bash
curl https://your-analytics-domain.com/stats/pages?api_key=your-api-key
```

will return something like:

```json
[
  {
    "path": "/posts/18",
    "day": "2024-11-07T00:00:00Z",
    "visitors": 1
  },
  {
    "path": "/about",
    "day": "2024-11-07T00:00:00Z",
    "visitors": 1
  },
  {
    "path": "/",
    "day": "2024-11-07T00:00:00Z",
    "visitors": 1
  }
]
```

## Contributing

Pull requests are welcome :)
