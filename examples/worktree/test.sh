#!/bin/bash
set -e
echo "Initializing worktree for testing..."
./zordon worktree create feature --agent
echo "Verifying worktree example..."
./zordon status --agent
./zordon stop --agent
EOF
chmod +x examples/worktree/test.sh