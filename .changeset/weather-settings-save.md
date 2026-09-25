---
"ftw": patch
---

Settings › Weather saves the forecast provider and PV rated power you choose.
The form showed `met_no` and 10000 W when neither was set, so accepting them
changed nothing and Save kept no provider and no rated power: the site got no
solar forecast. The form now shows what Core uses, no provider and an empty
rated power, and a select whose stored value is not one of its options shows
its default instead of the first option.
