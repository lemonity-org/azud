#!/bin/bash
# Security linting script for Azud
# Detects potentially unsafe patterns in SSH command construction
#
# Run this script as part of CI to prevent security regressions.

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "=== Azud Security Lint ==="
echo ""

ERRORS=0

# Pattern 1: fmt.Sprintf with SSH Execute that doesn't use shell.Quote
# This catches cases like: fmt.Sprintf("cat %s", path) used with ssh.Execute
echo "Checking for unquoted variables in SSH commands..."

# Look for fmt.Sprintf patterns near SSH execution that don't use shell.Quote
UNSAFE_PATTERNS=$(grep -rn 'fmt.Sprintf.*%s' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -v 'shell.Quote' | \
    grep -v 'state.DirQuoted\|state.LockFileQuoted\|state.ConfigFileQuoted' | \
    grep -E '(ssh|Execute|cmd :=|cmd =)' | \
    grep -v '// safe:' | \
    grep -v 'format\|Format\|template\|Template\|log\|Log\|error\|Error\|message\|Message' || true)

if [ -n "$UNSAFE_PATTERNS" ]; then
    echo -e "${YELLOW}Warning: Potential unquoted variables in SSH commands:${NC}"
    echo "$UNSAFE_PATTERNS"
    echo ""
    echo "If these are safe, add '// safe: <reason>' comment to suppress."
    # Don't fail on warnings, just alert
fi

# Pattern 2: Direct string concatenation in SSH commands
echo "Checking for string concatenation in SSH commands..."

CONCAT_PATTERNS=$(grep -rn 'Execute(.*+.*+' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -v 'shell.Quote' | \
    grep -v '// safe:' || true)

if [ -n "$CONCAT_PATTERNS" ]; then
    echo -e "${YELLOW}Warning: String concatenation in Execute calls:${NC}"
    echo "$CONCAT_PATTERNS"
    echo ""
fi

# Pattern 3: Check for credentials in command strings (passwords, tokens, secrets)
# This looks for actual credential VALUES being interpolated into commands
echo "Checking for potential credential exposure in commands..."

CRED_PATTERNS=$(grep -rn -i 'password\|apikey\|api_key' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -E 'fmt.Sprintf.*\$\{?[A-Za-z_]+\}?.*Execute|Execute.*fmt.Sprintf.*password' | \
    grep -v 'stdin\|Stdin\|STDIN\|--password-stdin' | \
    grep -v '// safe:' || true)

if [ -n "$CRED_PATTERNS" ]; then
    echo -e "${RED}Error: Potential credential exposure in commands:${NC}"
    echo "$CRED_PATTERNS"
    ERRORS=$((ERRORS + 1))
fi

# Pattern 4: Check that state.Dir() results aren't passed through shell.Quote()
echo "Checking for incorrect quoting of state paths..."

STATE_QUOTE_PATTERNS=$(grep -rn 'shell.Quote(state\.' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' || true)

if [ -n "$STATE_QUOTE_PATTERNS" ]; then
    echo -e "${RED}Error: state.Dir/LockFile/ConfigFile should not be passed to shell.Quote():${NC}"
    echo "$STATE_QUOTE_PATTERNS"
    echo "Use state.DirQuoted(), state.LockFileQuoted(), or state.ConfigFileQuoted() instead."
    ERRORS=$((ERRORS + 1))
fi

# Pattern 5: Check for 0.0.0.0 bindings (network exposure)
echo "Checking for exposed network bindings..."

BIND_PATTERNS=$(grep -rn '0\.0\.0\.0' --include="*.go" internal/ 2>/dev/null | \
    grep -v '_test.go' | \
    grep -v '// safe:' || true)

if [ -n "$BIND_PATTERNS" ]; then
    echo -e "${RED}Error: Found 0.0.0.0 bindings (potential network exposure):${NC}"
    echo "$BIND_PATTERNS"
    ERRORS=$((ERRORS + 1))
fi

# Run govulncheck for dependency vulnerabilities
echo ""
echo "Checking for dependency vulnerabilities..."

if command -v govulncheck &> /dev/null; then
    if ! govulncheck ./... 2>&1; then
        echo -e "${RED}Error: Vulnerability check failed${NC}"
        ERRORS=$((ERRORS + 1))
    else
        echo -e "${GREEN}No known vulnerabilities in dependencies${NC}"
    fi
else
    echo -e "${YELLOW}Warning: govulncheck not installed, skipping dependency check${NC}"
    echo "Install with: go install golang.org/x/vuln/cmd/govulncheck@latest"
fi

echo ""
echo "=== Security Lint Complete ==="

if [ $ERRORS -gt 0 ]; then
    echo -e "${RED}Found $ERRORS error(s)${NC}"
    exit 1
else
    echo -e "${GREEN}No security errors found${NC}"
    exit 0
fi
