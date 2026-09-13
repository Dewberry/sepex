#!/usr/bin/env bash

set -euo pipefail

# Script: generate-release-notes.sh
# Description: Generates release notes from CHANGELOG.md
# Usage: ./generate-release-notes.sh <tag_name> <github_repository> <github_output>
# Arguments:
#   - TAG_NAME: The tag name (e.g., v1.0.0)
#   - GITHUB_REPOSITORY: The GitHub repository (e.g., owner/repo)
#   - GITHUB_OUTPUT: Path to the GitHub Actions output file

TAG_NAME="${1:-}"
GITHUB_REPOSITORY="${2:-}"
GITHUB_OUTPUT="${3:-}"

if [ -z "$TAG_NAME" ] || [ -z "$GITHUB_REPOSITORY" ]; then
  echo "Error: Missing required arguments"
  echo "Usage: $0 <tag_name> <github_repository> <github_output>"
  exit 1
fi

echo "Generating release notes for $TAG_NAME..."

VERSION_NUM=${TAG_NAME#v}  # Remove 'v' prefix if present

# Extract changelog section (Keep a Changelog format)
if [ -f "CHANGELOG.md" ]; then
  echo "## What's Changed" >> release_notes.md
  echo "" >> release_notes.md

  # Look for version with ## [Version] format (Keep a Changelog standard)
  awk -v version="$VERSION_NUM" '
    BEGIN { found=0; in_section=0; release_title="" }

    # Match version headers with ## [version] format
    /^## \[/ {
      if (in_section) exit  # Stop at next version

      # Extract version from [version] - YYYY-MM-DD format
      match($0, /\[([^\]]+)\]/, arr)
      if (arr[1]) {
        ver = arr[1]
        # Remove v prefix if present
        gsub(/^v/, "", ver)

        if (ver == version) {
          found=1
          in_section=1
          # Extract the full line after ## [version]
          # Example: ## [0.2.1] - 2025-12-03 -> v0.2.1 - 2025-12-03
          # Example: ## [0.2.1] - 2025-12-03 - Title -> v0.2.1 - 2025-12-03 - Title
          sub(/^## \[/, "", $0)
          sub(/\]/, "", $0)
          release_title = "v" $0
          next  # Skip the header itself
        }
      }
    }

    # Print content if we are in the right section
    in_section {
      # Stop if we hit a new version header (## [)
      if (/^## \[/) exit
      # Skip the bottom link references
      if (/^\[.*\]:/) exit
      print
    }

    # Output found status and title at the end
    END {
      print "FOUND=" found
      print "RELEASE_TITLE=" release_title
    }
  ' CHANGELOG.md > changelog_section.tmp

  # Extract the FOUND status and RELEASE_TITLE from the last two lines
  RELEASE_TITLE=$(tail -n 1 changelog_section.tmp | grep "^RELEASE_TITLE=" | cut -d= -f2-)
  FOUND_STATUS=$(tail -n 2 changelog_section.tmp | head -n 1 | grep "^FOUND=" | cut -d= -f2)

  # Check if version was found in changelog
  if [ "$FOUND_STATUS" != "1" ]; then
    echo "Error: Version $VERSION_NUM not found in CHANGELOG.md"
    echo "Please add an entry for version $VERSION_NUM to CHANGELOG.md before creating a release."
    rm -f changelog_section.tmp
    exit 1
  fi

  # Output release title to GitHub Actions
  if [ -n "$GITHUB_OUTPUT" ] && [ -n "$RELEASE_TITLE" ]; then
    echo "release_title=$RELEASE_TITLE" >> "$GITHUB_OUTPUT"
  fi

  # Remove the last two lines (FOUND and RELEASE_TITLE status) from temp file
  head -n -2 changelog_section.tmp > changelog_section_clean.tmp
  mv changelog_section_clean.tmp changelog_section.tmp

  if [ -s changelog_section.tmp ]; then
    cat changelog_section.tmp >> release_notes.md
    echo "" >> release_notes.md
  else
    echo "Error: Changelog entry for version $VERSION_NUM is empty"
    rm -f changelog_section.tmp
    exit 1
  fi
  rm -f changelog_section.tmp
fi

echo "## Deploy" >> release_notes.md
echo "" >> release_notes.md


echo "### 🐳 Docker/Container" >> release_notes.md
echo "" >> release_notes.md
echo "\`\`\`bash" >> release_notes.md
echo "# Pull the image" >> release_notes.md
echo "docker pull ghcr.io/$GITHUB_REPOSITORY:${TAG_NAME#v}" >> release_notes.md
echo "" >> release_notes.md
echo "\`\`\`" >> release_notes.md
echo "" >> release_notes.md

echo "Release notes generated successfully: release_notes.md"