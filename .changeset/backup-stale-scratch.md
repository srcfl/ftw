---
"ftw": patch
---

Core now removes what a stopped full backup left behind before it accepts a new one. A backup cut short when Core stops, for example by an update, left its raw copy in /dev/shm until the next reboot. On a Raspberry Pi that held more than a gigabyte of RAM and sent the next backup's copy to the SD card, where it ran past its two-hour limit. At start Core removes such raw copies, the stage and verification folders and any unfinished archive in the backups folder, and logs each path and its size. It leaves them when another backup is running, and it removes nothing else.
