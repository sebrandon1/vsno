# Removes the builder's files for each vSNO named in the arguments.
set -euo pipefail
for name in "$@"; do
  rm -rf "/work/guests/$name" "/work/www/$name.iso" "/work/www/$name.iso.tmp"
done
