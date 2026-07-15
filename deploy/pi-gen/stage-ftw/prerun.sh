#!/bin/bash -e
# Standard pi-gen prerun: seed this stage's rootfs from the previous
# one. Without this the chroot scripts would run against an empty
# directory and every `apt-get` would fail to find /etc/apt.

if [ ! -d "${ROOTFS_DIR}" ]; then
    copy_previous
fi
