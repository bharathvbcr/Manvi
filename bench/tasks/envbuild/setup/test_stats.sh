#!/bin/sh
set -e
./build.sh
out=$(./stats 3 9 4)
[ "$out" = "sum=16 max=9" ] || { echo "got: $out want: sum=16 max=9"; exit 1; }
out=$(./stats 5)
[ "$out" = "sum=5 max=5" ] || { echo "got: $out want: sum=5 max=5"; exit 1; }
echo ok
