## Draytek Vigor Signal Stats server

- This is a simple HTTP server that returns an autorefreshing page with DSL stats to help when troubleshooting xDSL connections or build dashboards.
- Do not expose port 8080 in the firewall otherwise the whole world will be able to see your location and signal level. There is no access restriction of any kind. This port should be exposed internally only.
