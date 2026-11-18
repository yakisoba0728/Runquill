#!/bin/sh
set -eu

test "$#" -eq 2
test "$1" = --jitconfig
test "$2" = integration-fixture
test "$(id -u)" = 1001
test ! -e /runquill/config
test ! -e /runquill-secret/config
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
