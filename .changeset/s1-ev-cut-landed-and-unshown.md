---
"ftw": patch
---

The car's fuse cut now acts on what the site meter shows. A cut the meter already shows no longer counts as still on its way, so when the house adds load after it, FTW cuts again once the fuse's heat budget is spent instead of waiting up to three minutes. A house load that moves on one phase alone is not taken for the car's cut landing, so it no longer stacks a second cut on a slow charger. A raise that is on its way but no longer fits now comes off before it lands, when it would take a phase above 1.2 times the fuse rating, or over the line on a phase whose heat budget is spent. FTW judges how much of the raise has landed from the meter's smallest rise across the phases, so a house step on one phase does not hide a raise that has not landed.
