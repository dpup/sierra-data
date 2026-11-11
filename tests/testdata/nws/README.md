# NWS fixtures

Live api.weather.gov responses captured 2026-11-11 ~23:13Z (public products).

- `alert_wsw_caz139_20261110_replaced.json`: `/alerts/{id}` for the CAZ139 Winter
  Storm Watch issued 2026-11-10 (messageType Alert). Now carries `replacedBy`.
- `alert_wsw_caz139_20261111_update.json`: its re-issue, 2026-11-11 12:03 PST
  (messageType Update, `references` the alert above).
- `active_caz137-139_20261111.json`: `/alerts/active?zone=CAZ137,CAZ138,CAZ139&status=actual`
  at the same moment. Only the Update is listed.
- `alert_svr_20261111_cancel.json`: a Cancel message (Severe Thunderstorm
  Warning, Gibson County IN) with two `references`. `/alerts/active` does not
  list Cancel messages; this came from `/alerts?message_type=cancel`.
