# Output Contract

Azud output is a line-oriented operations record. Labels carry meaning; color
only reinforces state.

```text
  # Deploy / ghcr.io/acme/api:v42
  --------------------------------------------------------
  INFO   Deploying to 2 hosts
  STEP   2/7  Sync secrets
  HOST   app-01 / Starting container
  OK     app-01 / Container started
  WARN   Image digest verification disabled
  ERROR  app-02 / Readiness check failed
  CMD    podman build --pull .
         | STEP 1/8: FROM docker.io/library/alpine
```

## Render modes

| Destination | ANSI color | Structure | Width behavior |
|---|---|---|---|
| Interactive UTF-8 terminal | Theme-mapped ANSI | Unicode rules and gauges | Wide tables reflow when required |
| Interactive non-UTF-8 terminal | Theme-mapped ANSI | ASCII | Wide tables reflow when required |
| Pipe, file, command substitution, or CI log | None | ASCII | Stable column layout |
| `TERM=dumb` | None | ASCII | Stable column layout |
| `NO_COLOR` present | None | Terminal-appropriate | Otherwise unchanged |
| `CLICOLOR=0` | None | Terminal-appropriate | Otherwise unchanged |

Color is detected independently for stdout and stderr. Azud emits no spinners,
carriage-return progress, ornamental motion, or soft terminal effects. Body
text uses the terminal's default foreground. Classification labels, state,
technical gauges, and structural rules use the terminal's theme-mapped ANSI
palette so light, dark, and high-contrast themes retain control of legibility.

## Automation

Do not scrape the human display when a machine surface exists:

- `azud version --short` prints one unstyled version value.
- `azud env get <key>` prints the value only.
- `azud ssh trust --template` prints YAML only.
- app, accessory, proxy, and cron log/exec streams remain undecorated.
- `server exec` labels captured stdout and stderr beneath the relevant host.
- shell completion output remains undecorated.

Azud-generated non-TTY records use deterministic ASCII framing without ANSI;
UTF-8 values remain intact.
Direct log and exec streams can still contain bytes emitted by the child
process. Plain records are the default in GitHub Actions. The generated
deployment workflow also sets `NO_COLOR=1` on the Azud deploy step so the
contract is explicit.

## Status vocabulary

The following labels belong to the Azud CLI. Installer, Make, and security
scripts extend the same fixed-label grammar for their own operations.

| Label | Meaning | Functional color |
|---|---|---|
| `INFO` | Context or declared operation | Blue |
| `STEP` | Numbered pipeline stage | Blue |
| `HOST` | Host-scoped operation | Blue |
| `OK` | Completed or production-ready | Green |
| `WARN` | Attention, pending, or degraded | Yellow |
| `ERROR` | Failed operation or invalid state | Red |
| `DEBUG` | Verbose implementation detail | Gray |
| `CMD` | Local or remote command | Gray |
| `STATE` | Explicit state value | State-dependent |
| `SPLIT` | Canary/stable traffic allocation | Blue and green |
| `REC` | Reflowed table record | Blue |

The written label is authoritative. Color is never the only indication of
state.
