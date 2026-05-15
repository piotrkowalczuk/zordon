#!/bin/bash
set -e
echo "Verifying simple example..."
./zordon status --agent
./zordon stop --agent
EOF
chmod +x examples/simple/test.sh