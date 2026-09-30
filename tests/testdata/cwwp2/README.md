# Caltrans CWWP2 fixtures

Captures of the CWWP2 data portal (`https://cwwp2.dot.ca.gov/data/d{N}/...`),
parsed by `internal/clients/cwwp2`. Captured 2026-09-30 ~03:55 UTC (20:55 PDT).
`make fetch-cwwp2-data` saves fresh timestamped copies. These fixtures are
committed and tests depend on their exact counts, so don't overwrite them.

| File | What | Edited? |
|---|---|---|
| `cc_d10_20260930.json` | District 10 chain controls: all 149 checkpoints, every one `R-0`. A quiet September day. | **Verbatim** |
| `lcs_d10_20260930.json` | District 10 lane closures, trimmed from 370 rows to 84. Keeps every row whose begin county is Alpine, Amador, Calaveras, Mariposa or Tuolumne, plus two rows of any 10-97/10-98/10-22/indefinite combination not otherwise present. | Rows removed, then re-serialized. Row content is untouched. |
| `cc_d07_bad_status_20260930.json` | District 7: one healthy checkpoint plus the two SR-2 checkpoints whose `status` held a **longitude** (`"-118.1307759"`). This is a real upstream defect. | Rows removed, then re-serialized. Row content is untouched. |
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
  (`MAX`/`MIN`/`TS`). Until this is known, `cc.kml` stays in use for exactly
  those entries.

At the first real storm, capture `make fetch-cwwp2-data` alongside
`make fetch-caltrans-data`. Then replace the synthetic fixture with the real
one, and resolve the two questions above.

## Portal quirks seen while capturing

- D4, D5 and D12 chain-control files answered HTTP 500.
- D11's chain-control file is not valid UTF-8.
- The road-weather (`rwis`) JSON drops the comma between repeated sensor
  entries, so it fails to parse. Its XML variant is well-formed. D10's RWIS
  stations are all Valley fog/wind sites, so that feed is of no use to us.
- The file name zero-pads the district (`ccStatusD03.json`) but the directory
  does not (`/d3/`). `ccStatusD3.json` answers 500.
- `recordDate`/`recordTime` are Pacific local time. The `*Epoch` fields in the
  lane-closure feed are real Unix epochs.
