# FTW licensing

This version uses GNU AGPL v3 only, with the narrow Energyplan combination
permission in [LICENSE](LICENSE). [NOTICE](NOTICE) preserves prior Apache
rights and attribution. Third-party files retain their own licenses.

## Use and redistribution

You may inspect, run, modify and sell the AGPL-covered software. When you
convey a covered work, provide its Corresponding Source under the AGPL.
When you modify it and let users interact with it remotely over a network,
offer those users that version's Corresponding Source at no charge as section
13 requires. A separate program does not become AGPL merely by sharing a
distribution or communicating over an API; the actual combination matters.
These paragraphs explain the license; LICENSE contains the binding terms.

Sourceful Labs AB may offer separate commercial terms for rights it controls.
Such a contract must identify the software and rights covered. It does not
replace third-party licenses or change earlier grants. A DCO sign-off is not
a copyright assignment or a general right to offer proprietary licenses.

## Energyplan

Energyplan's proprietary binaries have a separate license. The permission in
LICENSE allows the specified combination without requiring Energyplan source;
it does not license the binary or extend that exception to other closed code.
The binary license bundled with the exact worker governs its use. New workers
under the Energyplan Home Use Binary License allow private household use with
FTW and free, noncommercial redistribution for that use. Commercial use,
bundling, resale and services require a separate Sourceful license. Earlier
workers keep their earlier grants. None of these restrictions apply to FTW's
AGPL-covered code when used without those restricted binaries.

Before upgrading a commercial installation to a release carrying the new
workers, obtain the required Energyplan license or use an FTW deployment that
omits them. An automatic update does not grant commercial rights to a new
worker. Keep the original license with any older worker you retain.

In image metadata, `LicenseRef-Energyplan-Home-Use-1.0` means the license in
`optimizer/native/bundle/LICENSE.txt`. The Core image lists both AGPL and
Energyplan terms; the updater lists AGPL. The AGPL grant includes the extra
permission in LICENSE. Dependency licenses remain in their own notices.

## Source for a distributed version

The source repository is https://github.com/srcfl/ftw.
Use the running build's commit or release tag, not the moving default branch,
to obtain matching source. Download an archive from that revision's Code menu,
or clone the repository and check out the revision. Include build and install
scripts, dependency lockfiles, license notices and any required installation
information. FTW Core's driver pin identifies its separately supplied driver
source and that snapshot's license; the bundled driver scripts are also source
code. An older pinned snapshot retains its earlier license.

Distributors must publish all their changes and any additional source needed
to meet the license, keep the source available for the required period, and
update source links to their own matching source. An upstream link alone is
not a source offer for a modified fork or an uncommitted build. For consumer
devices, meet AGPL section 6's Installation Information requirements where
they apply. Publishing source does not require publishing user data, secrets
or Energyplan source covered by the combination permission.
