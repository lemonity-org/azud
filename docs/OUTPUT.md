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
- app, accessory, proxy, and cron log/exec streams remain undecorated; see
  [Remote output](#remote-output) for how control sequences are handled.
- `server exec` labels captured stdout and stderr beneath the relevant host.
- shell completion output remains undecorated.

Azud-generated non-TTY records use deterministic ASCII framing without ANSI;
UTF-8 values remain intact. Plain records are the default in GitHub Actions.
The generated deployment workflow also sets `NO_COLOR=1` on the Azud deploy
step so the contract is explicit.

## Remote output

Container logs, exec output, and remote command diagnostics are untrusted: a
compromised workload controls those bytes. Azud neutralizes terminal control
sequences before printing them so a workload cannot write the clipboard (OSC
52), clear the screen, move the cursor, or forge Azud records.

Neutralizing means: SGR color sequences are removed; CRLF and lone CR become
LF; newline and tab are kept; every other control byte, escape sequence, C1
control, invalid UTF-8 byte, and Unicode bidi override is shown as visible
text such as `\x1b]52;c;` or `\u202e`. UTF-8 text is otherwise unchanged.

| Surface | Policy |
|---|---|
| `app logs`, `accessory logs`, `proxy logs`, `cron logs` (including `-f`) | Neutralized on every destination, including pipes and files |
| `app exec`, `accessory exec` without `-i`/`-t` | Neutralized only when the stream is a terminal; piped or redirected output stays byte-exact for dumps and archives |
| `app exec -i`/`-t` | Never filtered: an interactive session is full terminal access to the container |
| `server exec` captured output and every Azud record (`INFO`, `ERROR`, ...), including errors that quote remote stderr | Always neutralized |

Pass `--raw` to a log or non-interactive exec command to print remote bytes
unchanged, for example to keep an application's own colors. `NO_COLOR` and
`CLICOLOR` only control Azud's styling; they do not change this policy.

Neutralizing control bytes does not stop plain-text output that a CI system
interprets, such as GitHub Actions workflow commands (`::add-mask::`) printed by
a container. Treat log output in CI as untrusted text.

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
