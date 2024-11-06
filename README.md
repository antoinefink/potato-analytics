# 🥔 Potato Analytics

A lightweight and minimalistic analytics solution that tracks page views while respecting user privacy. Potato Analytics only tracks daily unique page views, nothing else. It makes it super light, fast, and saves you from doing any maintenance.

Other key features are:

- Lightweight (< 1KB compressed)
- Privacy friendly (doesn't store any personal information)
- Uses caching to minimize unnecessary requests
- Automatically filters out bots and crawlers
- Accessible via a simple API
- Uses a probabilistic data structure called [HyperLogLog](https://en.wikipedia.org/wiki/HyperLogLog) to estimate the number of unique visitors which saves on storage

## Usage

Add the following script to your website's HTML (ideally just before the closing `</body>` tag):

```html
<script src="https://your-analytics-domain.com/analytics.js" async></script>
```

## Deployment

The easiest way to deploy Potato Analytics is to use the Docker image.

TBD for the rest.
