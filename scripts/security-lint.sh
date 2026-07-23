#!/bin/bash
# Security checks for SSH command construction and reachable vulnerabilities.

set -e

BLUE=''
GREEN=''
RED=''
RESET=''
if [[ -t 1 && "${TERM:-}" != "dumb" && "${CLICOLOR:-1}" != "0" && -z "${NO_COLOR+x}" ]]; then
    BLUE=$'\033[0;34m'
    GREEN=$'\033[0;32m'
    RED=$'\033[0;31m'
    RESET=$'\033[0m'
fi

record() {
    local state=$1
    local color=$2
    shift 2
    printf '  %b%-5s%b  %s\n' "$color" "$state" "$RESET" "$*"
}

details() {
    printf '%s\n' "$1" | sed 's/^/         | /'
}

printf 'SECURITY / STATIC CHECKS\n'
printf '%s\n' '--------------------------------------------------------'

ERRORS=0

# Check fmt.Sprintf values used near SSH execution without shell quoting.
record CHECK "$BLUE" "SSH command interpolation"
UNSAFE_PATTERNS=$(grep -rn 'fmt.Sprintf.*%s' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -v 'shell.Quote' | \
    grep -v 'state.DirQuoted\|state.LockFileQuoted\|state.ConfigFileQuoted' | \
    grep -E '(ssh|Execute|cmd :=|cmd =)' | \
    grep -v '// safe:' | \
    grep -v 'format\|Format\|template\|Template\|log\|Log\|error\|Error\|message\|Message' || true)

if [[ -n "$UNSAFE_PATTERNS" ]]; then
    record ERROR "$RED" "Potential unquoted variables in SSH commands"
    details "$UNSAFE_PATTERNS"
    record INFO "$BLUE" "If safe, add // safe: <reason> to the source line"
    ERRORS=$((ERRORS + 1))
else
    record PASS "$GREEN" "No unsafe interpolation found"
fi

# Check direct concatenation in SSH Execute calls.
record CHECK "$BLUE" "SSH command concatenation"
CONCAT_PATTERNS=$(grep -rn 'Execute(.*+.*+' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -v 'shell.Quote' | \
    grep -v '// safe:' || true)

if [[ -n "$CONCAT_PATTERNS" ]]; then
    record ERROR "$RED" "String concatenation in Execute calls"
    details "$CONCAT_PATTERNS"
    ERRORS=$((ERRORS + 1))
else
    record PASS "$GREEN" "No unsafe concatenation found"
fi

# Check whether credential values can enter process arguments.
record CHECK "$BLUE" "Credential exposure in commands"
CRED_PATTERNS=$(grep -rn -i 'password\|apikey\|api_key' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -E 'fmt.Sprintf.*\$\{?[A-Za-z_]+\}?.*Execute|Execute.*fmt.Sprintf.*password' | \
    grep -v 'stdin\|Stdin\|STDIN\|--password-stdin' | \
    grep -v '// safe:' || true)

if [[ -n "$CRED_PATTERNS" ]]; then
    record ERROR "$RED" "Potential credential values in process arguments"
    details "$CRED_PATTERNS"
    ERRORS=$((ERRORS + 1))
else
    record PASS "$GREEN" "No credential argument exposure found"
fi

# State helpers already return quoted paths.
record CHECK "$BLUE" "State-path quoting"
STATE_QUOTE_PATTERNS=$(grep -rn 'shell.Quote(state\.' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' || true)

if [[ -n "$STATE_QUOTE_PATTERNS" ]]; then
    record ERROR "$RED" "State paths passed through shell.Quote"
    details "$STATE_QUOTE_PATTERNS"
    record INFO "$BLUE" "Use state.DirQuoted, state.LockFileQuoted, or state.ConfigFileQuoted"
    ERRORS=$((ERRORS + 1))
else
    record PASS "$GREEN" "State-path quoting is valid"
fi

# Check accidental wildcard network exposure.
record CHECK "$BLUE" "Wildcard network bindings"
BIND_PATTERNS=$(grep -rn '0\.0\.0\.0' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -v '// safe:' || true)

if [[ -n "$BIND_PATTERNS" ]]; then
    record ERROR "$RED" "Found 0.0.0.0 bindings"
    details "$BIND_PATTERNS"
    ERRORS=$((ERRORS + 1))
else
    record PASS "$GREEN" "No unreviewed wildcard bindings found"
fi

record CHECK "$BLUE" "Reachable dependency vulnerabilities"
if command -v govulncheck >/dev/null 2>&1; then
    if VULN_OUTPUT=$(govulncheck ./... 2>&1); then
        record PASS "$GREEN" "No reachable vulnerabilities found"
    else
        record ERROR "$RED" "Vulnerability check failed"
        details "$VULN_OUTPUT"
        ERRORS=$((ERRORS + 1))
    fi
else
    record ERROR "$RED" "govulncheck is required"
    record INFO "$BLUE" "Install: go install golang.org/x/vuln/cmd/govulncheck@v1.6.0"
    ERRORS=$((ERRORS + 1))
fi

printf '\nSECURITY / RESULT\n'
printf '%s\n' '--------------------------------------------------------'
if (( ERRORS > 0 )); then
    record ERROR "$RED" "$ERRORS check(s) failed"
    exit 1
fi

record PASS "$GREEN" "All security checks passed"
