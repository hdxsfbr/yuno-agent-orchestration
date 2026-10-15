#!/usr/bin/env bash
# Mutation check for one rule: break the code on purpose, run the test that covers the
# rule, restore the file. The test must fail on the broken code ("killed"); a test that
# still passes ("survived") does not cover the rule.
#
#   scripts/mutation-check.sh -f internal/engine/walk.go \
#     -o 'if id != v.ocID' -r 'if false' \
#     -c "docker compose --profile tools run --rm --no-deps go go test ./internal/engine/ -run TestX -count=1"
#
# -o is a literal that must occur exactly once in the file (use $'..' for newlines); -r
# replaces it. -c runs on the host, so toolchains still run in Docker. -l names the log
# (default: a temp file). Exit status: 0 killed, 1 survived, 2 usage or setup error.
set -euo pipefail

file= orig= repl= cmd= log=
while getopts "f:o:r:c:l:" opt; do
  case $opt in
    f) file=$OPTARG ;;
    o) orig=$OPTARG ;;
    r) repl=$OPTARG ;;
    c) cmd=$OPTARG ;;
    l) log=$OPTARG ;;
    *) exit 2 ;;
  esac
done
if [[ -z $file || -z $orig || -z $cmd ]]; then
  echo "usage: $0 -f FILE -o ORIGINAL -r REPLACEMENT -c COMMAND [-l LOG]" >&2
  exit 2
fi
[[ -f $file ]] || { echo "no such file: $file" >&2; exit 2; }
log=${log:-$(mktemp "${TMPDIR:-/tmp}/mutation.XXXXXX.log")}

backup=$(mktemp "${TMPDIR:-/tmp}/mutation-backup.XXXXXX")
cp -p "$file" "$backup"
restore() { cp -p "$backup" "$file" && rm -f "$backup"; }
trap restore EXIT

count=$(ORIG=$orig perl -0777 -ne '$n = () = /\Q$ENV{ORIG}\E/g; print $n' "$file")
if [[ $count != 1 ]]; then
  echo "the original text occurs $count times in $file; it must occur once" >&2
  exit 2
fi
ORIG=$orig REPL=$repl perl -0777 -i -pe '
  my $i = index($_, $ENV{ORIG});
  substr($_, $i, length $ENV{ORIG}) = $ENV{REPL};
' "$file"

echo "mutated $file; running: $cmd (log: $log)"
if bash -c "$cmd" > "$log" 2>&1 < /dev/null; then
  echo "SURVIVED: the command passed on the mutated code"
  exit 1
fi
echo "KILLED: the command failed on the mutated code"
grep -E -- '^(--- FAIL|FAIL|ok )|FAIL|✗|×' "$log" | head -15 || true
