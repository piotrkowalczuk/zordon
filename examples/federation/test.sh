#!/bin/bash
set -e
echo "Verifying federation example..."
./zordon status --agent
./zordon stop --agent
EOF
chmod +x examples/federation/test.sh