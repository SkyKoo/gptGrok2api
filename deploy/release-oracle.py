#!/usr/bin/env python3
"""Compatibility entry point for release.py."""

import subprocess
import sys

from release import main


if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as error:
        sys.exit(error.returncode)
