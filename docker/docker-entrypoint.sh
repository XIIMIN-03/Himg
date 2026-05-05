#!/bin/sh
set -eu

mkdir -p /data/uploads /themes

if [ ! -d /themes/default ]; then
    mkdir -p /themes/default
    cp -R /opt/himg-defaults/themes/default/. /themes/default/
fi

exec "$@"
