# Caltrans CWWP2 fixtures

Captures of the CWWP2 data portal (`https://cwwp2.dot.ca.gov/data/d{N}/...`),
parsed by `internal/clients/cwwp2`. The `cc`/`lcs` files were captured
2026-09-30 ~03:55 UTC (20:55 PDT), the `cms` files 2026-10-01 ~19:45 UTC
(12:45 PDT). `make fetch-cwwp2-data` saves fresh timestamped copies. These
fixtures are committed and tests depend on their exact counts, so don't
overwrite them.

| File | What | Edited? |
|---|---|---|
| `cc_d10_20260930.json` | District 10 chain controls: all 149 checkpoints, every one `R-0`. A quiet September day. | **Verbatim** |
| `lcs_d10_20260930.json` | District 10 lane closures, trimmed from 370 rows to 84. Keeps every row whose begin county is Alpine, Amador, Calaveras, Mariposa or Tuolumne, plus two rows of any 10-97/10-98/10-22/indefinite combination not otherwise present. | Rows removed, then re-serialized. Row content is untouched. |
| `cc_d07_bad_status_20260930.json` | District 7: one healthy checkpoint plus the two SR-2 checkpoints whose `status` held a **longitude** (`"-118.1307759"`). This is a real upstream defect. | Rows removed, then re-serialized. Row content is untouched. |
| `cms_d10_20261001.json` | District 10 message signs: all 107. 82 show one statewide safety campaign, 14 are dark, 2 are `Not Reported` (one out of service, one stamped `1970-01-01`). | **Verbatim** |
| `cms_d07_frozen_20261001.json` | District 7, from a file **frozen since 2026-09-29 05:32 PDT** but still served. Five signs: a bare `&` in the text, a two-page message, a dark sign, a `Not Reported` one. Every message time is `Not Reported`. | Rows removed, then re-serialized. Row content is untouched. |
| `cms_d03_quirks_20261001.json` | District 3: `inService` as `True`/`False`, and lines padded with spaces (`" .US 50 22 MIN"`, `"SR 20 "`). | Rows removed, then re-serialized. Row content is untouched. |
| `cms_d02_quirks_20261001.json` | District 2: the two rows with a blank `inService`, one of them (index `0`) with an entirely blank location. | Rows removed, then re-serialized. Row content is untouched. |
| `cctv_d10_20261001.json` | District 10 cameras, trimmed from 154 rows to 13, captured 2026-10-01 ~19:40 UTC. Keeps the 4 in-area cameras (`d10-172` Soulsbyville, `-136` Pine Grove, `-129` Ferretti Rd, `-152` Buck Meadows), the 2 Mariposa SR-140 ones, all 3 out-of-service rows (`-41`, `-65`, `-76`), an image-only row (`-47`) and 3 Valley rows. | Rows removed, then re-serialized. Row content is untouched. |
| `cctv_d9_20261001.json` | District 9 cameras, 5 of 23: Sonora Junction (`d9-47`, US-395/SR-108), Bridgeport, Conway Summit, the out-of-service Matthieu Hill (`d9-48`) and US-6 State Line, which carries a description. D9 publishes no streams. | Rows removed, then re-serialized. Row content is untouched. |
| `cctv_d3_20261001.json` | District 3 cameras, 5 of 275: Echo Summit and Wrights Lake (US-50, the nearest D3 cameras to our area), a "Not Reported" refresh rate with a leading space in its name (`d3-238`), an image-only row (`-130`), and `-328`, flagged out of service while serving live images. | Rows removed, then re-serialized. Row content is untouched. |
| `cc_d10_synthetic_storm.json` | **SYNTHETIC.** The D10 capture with 6 Hwy 4 checkpoints (Arnold, Big Trees Park, Dorrington, Cottage Springs) set to `R-2` and Hwy 108 Pinecrest EB set to `R-1`. The requirement text is copied from the real 2025-12-24 `cc.kml` capture. | **Hand-edited** |

## What we have not seen yet (winter TODO)

Every status observed on 2026-09-30, across all districts that answered, was
`R-0` or garbage. **No real active chain control has been captured from CWWP2.**
So the following are still unverified:

- The exact `status` spelling for active controls. The parser accepts `R-1`,
  `R1` and `r-1`. Anything else parses as `LevelUnknown`, which degrades the
  layer to STALE rather than guessing.
- Whether CWWP2 reports **road closures** (the seasonal Ebbetts/Sonora/Tioga
  closures `cc.kml` shows as "Road Closed") or the **truck-only** levels
  (`MAX`/`MIN`/`TS`), and what status a closed gate carries.
- **Whether the two sources agree in a storm.** The `chain_control` layer logs
  `cc.kml reports chain controls CWWP2 does not` whenever they disagree.

Message signs (`cms`) have the same gap: every D10 message captured so far is
a safety campaign, a wind warning, slow traffic, or a work zone. **No storm message (chain
control, closure) has been captured.** We do not yet know how Caltrans words
them, whether they use one or two pages, or how many signs carry the same
message at once. Those answers decide how to tell a real message from
boilerplate.

At the first real storm, capture `make fetch-cwwp2-data` alongside
`make fetch-caltrans-data`. Then replace the synthetic fixture with the real
one, and resolve the questions above.

## What the 2025 cc.kml capture already tells us

Matched against `../caltrans/chain_controls_20251224.kml` (a real storm):

- **Every cc.kml point sits 0 m from a same-named CWWP2 checkpoint**, pass
  closure gates included (`MOUNT REBA ROAD - EBBETTS PASS`, `KENNEDY MEADOWS -
  SONORA PASS`, `CRANE FLAT RD (closure gate…)`). One shared registry. The one
  exception, `Highway 108 R-1 3.8 Mi. W of Jct. 395`, is in District 9.
- **But cc.kml carries `District:N Message ID:NNNN`**, the Highway Information
  (CHIN) identifier scheme, not CWWP2's checkpoint index. Its *status* may come
  from a different system. That's why the two are merged and never ranked.
- **CWWP2 can list two checkpoints at one spot** (`RED LAKE CREEK` and
  `RED LAKE CREEK - CARSON PASS`, 1 m apart, both westbound).

The portal regenerated `cc` files within about a minute of the wall clock in
captures at 17:20 and 20:57 PDT. Overnight regeneration is unverified; the
1-hour staleness check would fail loudly (not silently) if it pauses.

## Portal quirks seen while capturing

- D4, D5 and D12 chain-control files answered HTTP 500.
- D11's chain-control file is not valid UTF-8.
- The road-weather (`rwis`) JSON drops the comma between repeated sensor
  entries, so it fails to parse. Its XML variant is well-formed. D10's RWIS
  stations are all Valley fog/wind sites, so that feed is of no use to us.
- Out-of-service cameras serve a "Down for Construction" placeholder that is
  regenerated every cycle, with a current burned-in timestamp and a fresh
  Last-Modified. Image age therefore cannot tell a dead camera from a live one.
  The `cctv` file itself has no generation stamp: `recordTimestamp` is the
  camera record's edit date.
- The file name zero-pads the district (`ccStatusD03.json`) but the directory
  does not (`/d3/`). `ccStatusD3.json` answers 500.
- `recordDate`/`recordTime` are Pacific local time. The `*Epoch` fields in the
  lane-closure feed are real Unix epochs.
