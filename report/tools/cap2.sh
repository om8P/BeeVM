cd /mnt/c/Users/ommga/Desktop/ACC/BeeVM
export PATH=$PATH:/usr/local/go/bin
RAW=report/assets/raw
cap() { local name="$1"; shift; { printf "%s %s
" "$(date "+%Y-%m-%d %H:%M:%S")" "$ $*"; "$@" 2>&1; } > "$RAW/$name.txt"; }
cap 20-go-test bash -c 'go test -count=1 ./... 2>&1'
cap 21-go-test-count bash -c 'go test -count=1 -v ./... 2>&1 | grep -c "^--- PASS" | xargs echo "unit tests passed:"; go test -count=1 -v ./... 2>&1 | grep -c "^--- FAIL" | xargs echo "unit tests failed:"'
