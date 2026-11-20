#!/bin/sh
set -eu

test "$#" -eq 2
test "$1" = --jitconfig
test "$2" = upgrade-fixture-jit
test "$(id -u)" = 1001
echo runquill-upgrade-job-executed
