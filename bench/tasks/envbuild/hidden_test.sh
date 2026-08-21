#!/bin/sh
# Build from clean and check behaviour the visible test does not cover.
cd "$1" || exit 1
rm -f stats
sh ./build.sh >/dev/null 2>&1 || { echo "FAIL: build.sh failed"; exit 1; }
[ -x ./stats ] || { echo "FAIL: no stats binary"; exit 1; }
bad=0
check() {
  got=$(./stats $1 2>&1)
  [ "$got" = "$2" ] || { echo "FAIL stats $1 -> '$got' want '$2'"; bad=1; }
}
check "3 9 4"      "sum=16 max=9"
check "5"          "sum=5 max=5"
check "1 2 3 4 5"  "sum=15 max=5"
check "7 7 7"      "sum=21 max=7"
check "10 2"       "sum=12 max=10"
check "2 10"       "sum=12 max=10"
exit $bad
