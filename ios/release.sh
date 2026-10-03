#!/bin/sh
# Archives GHOSTWIRE for the App Store and uploads it to App Store Connect.
#
# Needs Xcode signed in to your Apple developer account (Xcode → Settings →
# Accounts) and your team ID (developer.apple.com → Membership).
#
#   TEAM_ID=ABCDE12345 ./release.sh            archive and upload
#   TEAM_ID=ABCDE12345 ./release.sh --export   archive and export an .ipa only
#
# The build number is the current date and time, so every upload is unique.
set -eu
cd "$(dirname "$0")"

: "${TEAM_ID:?Set TEAM_ID to your Apple Developer team ID}"
BUILD=${BUILD:-$(date +%Y%m%d%H%M)}
DEST=upload
[ "${1:-}" = "--export" ] && DEST=export
OUT=build

rm -rf "$OUT"
mkdir -p "$OUT"
sed -e "s/__TEAM_ID__/$TEAM_ID/" -e "s/__DEST__/$DEST/" ExportOptions.plist > "$OUT/ExportOptions.plist"

xcodebuild -project GHOSTWIRE.xcodeproj -scheme GHOSTWIRE -configuration Release \
  -destination 'generic/platform=iOS' -archivePath "$OUT/GHOSTWIRE.xcarchive" \
  DEVELOPMENT_TEAM="$TEAM_ID" CURRENT_PROJECT_VERSION="$BUILD" \
  -allowProvisioningUpdates archive

xcodebuild -exportArchive -archivePath "$OUT/GHOSTWIRE.xcarchive" \
  -exportOptionsPlist "$OUT/ExportOptions.plist" -exportPath "$OUT" \
  -allowProvisioningUpdates

if [ "$DEST" = upload ]; then
  echo "Uploaded build $BUILD. It appears in App Store Connect → TestFlight after processing (usually 5–30 minutes)."
else
  echo "Exported $OUT/GHOSTWIRE.ipa (build $BUILD)."
fi
