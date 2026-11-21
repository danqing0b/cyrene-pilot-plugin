#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Launch an unmodified Navigator checkout with the Pilot Work bridge."""
import argparse
import importlib.util
import json
from pathlib import Path
import sys


def main():
    parser = argparse.ArgumentParser(description=__doc__, add_help=False)
    parser.add_argument('--navigator-root', type=Path, required=True)
    parser.add_argument('--pilot-bindings', type=Path, required=True)
    args, remaining = parser.parse_known_args()
    root = args.navigator_root.absolute()
    sys.path.insert(0, str(root / 'src'))
    from pilot_bridge import attach_pilot
    spec = importlib.util.spec_from_file_location('pilot_navigator_launcher', root / 'scripts/serve-persistence.py')
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    config = json.loads(args.pilot_bindings.read_text())
    if not isinstance(config, dict) or set(config) != {'pilot_bindings'}:
        raise ValueError('Expected a pilot_bindings document')
    module.create_persistence_app = attach_pilot(module.create_persistence_app, config['pilot_bindings'])
    sys.argv = [str(root / 'scripts/serve-persistence.py'), *remaining]
    module.main()


if __name__ == '__main__':
    main()
