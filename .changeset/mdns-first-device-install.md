---
"ftw": patch
---

The setup wizard now installs a discovered device by its self-broadcast mDNS
(.local) name instead of its raw IP when the device advertises one, so the
connection survives DHCP lease changes. When only an IP address is used, the
wizard and device settings now tell the operator to reserve that IP for the
device in the router's DHCP settings.
