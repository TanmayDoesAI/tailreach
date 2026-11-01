#!/bin/sh
# Creates a self-signed code-signing identity "mydevices local" in your login keychain.
# Signing with a stable identity lets macOS keep the Screen Recording / Accessibility
# permissions you grant across rebuilds (ad-hoc signatures change on every build).
set -e
NAME="mydevices local"
if security find-certificate -c "$NAME" >/dev/null 2>&1; then echo "✓ '$NAME' already exists"; exit 0; fi
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/cfg" <<CFG
[req]
distinguished_name = dn
x509_extensions = ext
prompt = no
[dn]
CN = $NAME
[ext]
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
basicConstraints = critical, CA:false
CFG
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -keyout "$TMP/key.pem" -out "$TMP/cert.pem" -config "$TMP/cfg" 2>/dev/null
# OpenSSL 3 needs -legacy for a file macOS can import; macOS's own LibreSSL has no such option.
LEGACY=""
openssl pkcs12 -help 2>&1 | grep -q -- -legacy && LEGACY="-legacy"
openssl pkcs12 -export $LEGACY -inkey "$TMP/key.pem" -in "$TMP/cert.pem" -name "$NAME" -out "$TMP/id.p12" -passout pass:mydevices
security import "$TMP/id.p12" -k "$HOME/Library/Keychains/login.keychain-db" -P mydevices -T /usr/bin/codesign >/dev/null
echo "✓ created '$NAME' (login keychain)"
