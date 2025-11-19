#!/bin/bash

# Softwares like Dbeaver and QGIS can't open sqlite db saved in WSL due to special characters in path
# This script moves .data/db.sqlite file to the provided mounted location and then make a symbolic link to that location
# Usage ./move_db_to_mount.sh /mnt/c/Users/asiddiqui/.data/sepex

COPY_TO_PATH=$1

mkdir -p "$COPY_TO_PATH" || exit
mv -f .data/api/db.sqlite ${COPY_TO_PATH} # if this fails, make sure you are not connected to the DB from other softwares
ln -s  ${COPY_TO_PATH}/db.sqlite .data/api/db.sqlite


