#!/bin/bash
#
# go install github.com/tc-hib/go-winres@latest
# sudo apt install jq wixl msitools gh
#
# Releases a new version of ssh-cloud:
#
#   ./release.sh          # 0.1.0 -> 0.1.1 (build, the default)
#   ./release.sh minor    # 0.1.1 -> 0.2.0
#   ./release.sh major    # 0.2.0 -> 1.0.0
#
# The version is file_version in winres/winres.json, as
# "major.minor.build.revision". Windows Installer only compares the first
# three numbers, so every release raises at least the build. The revision
# goes up on every release as well.
#
# The script builds dist/ssh-cloud-<version>-amd64.msi, commits and pushes the
# version bump and creates the GitHub release v<version> with the MSI.
set -e
cd "$(dirname "$0")"

part=${1:-build}
case $part in major | minor | build) ;; *)
	echo "usage: $0 [major|minor|build]" >&2
	exit 1
	;;
esac
if [ -n "$(git status --porcelain)" ]; then
	echo "working tree not clean, commit first" >&2
	exit 1
fi

# Bump the version.
json=winres/winres.json
ver=$(jq -r '.RT_VERSION["#1"]["0000"].fixed.file_version' $json)
IFS=. read -r major minor build revision <<<"$ver"
case $part in
major) major=$((major + 1)) minor=0 build=0 ;;
minor) minor=$((minor + 1)) build=0 ;;
build) build=$((build + 1)) ;;
esac
revision=$((revision + 1))
version=$major.$minor.$build
jq --arg v "$version" --arg fv "$version.$revision" '
  .RT_VERSION["#1"]["0000"].fixed.file_version = $fv |
  .RT_VERSION["#1"]["0000"].fixed.product_version = $fv |
  .RT_VERSION["#1"]["0000"].info["0409"].FileVersion = $v |
  .RT_VERSION["#1"]["0000"].info["0409"].ProductVersion = $v
' $json >$json.tmp && mv $json.tmp $json
echo "Releasing ssh-cloud $version (revision $revision)"

# Build the exe: icon, manifest and version info (cmd/ssh-cloud/rsrc_windows_amd64.syso,
# not committed) and the version, which the app logs at startup.
~/go/bin/go-winres make --arch amd64 --out cmd/ssh-cloud/rsrc
go test ./...
GOOS=windows go vet ./...
rm -rf dist && mkdir dist
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-H windowsgui -s -w -X main.version=$version" -o dist/ssh-cloud.exe ./cmd/ssh-cloud

# Build the MSI.
msi=dist/ssh-cloud-$version-amd64.msi
go run ./tools/mkico dist/ssh-cloud.ico winres/icon.png winres/icon48.png winres/icon32.png winres/icon16.png
wixl -a x64 -D Version="$version" -D FileVersion="$version.$revision" \
	-D ExePath=dist/ssh-cloud.exe -D IconPath=dist/ssh-cloud.ico \
	-o $msi installer/ssh-cloud.wxs

# wixl removes the old version right after InstallValidate, outside the
# installation transaction: if the new install then fails, nothing is left.
# Move RemoveExistingProducts after InstallInitialize, so a failed upgrade
# rolls back to the old version (WiX: Schedule="afterInstallInitialize").
msiinfo export $msi InstallExecuteSequence >dist/seq.idt
sed -i 's/^RemoveExistingProducts\t\t1401\r\?$/RemoveExistingProducts\t\t1501\r/' dist/seq.idt
grep -q $'^RemoveExistingProducts\t\t1501\r$' dist/seq.idt
msibuild $msi -i dist/seq.idt
rm dist/seq.idt
# The exe must be versioned in the File table, see installer/ssh-cloud.wxs.
msiinfo export $msi File | grep -q $'\t'"$version.$revision"$'\t'
ls -l $msi

# Commit, push and publish.
git commit -q -m "Release v$version" $json
git push
gh release create v$version --title v$version $msi
