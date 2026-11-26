"""Collect license/notice files from modules used by the gateway build.

Usage: python tools/collect_licenses.py --go /path/to/go
Respects GOMODCACHE/GOCACHE. Requires dependencies already downloaded.
"""
import argparse
import json
from pathlib import Path
import subprocess


def main():
    args = argparse.ArgumentParser()
    args.add_argument('--go', default='go')
    opts = args.parse_args()
    raw = subprocess.check_output([opts.go, 'list', '-deps', '-json', './cmd/cloudreve-s3'], text=True, encoding='utf-8')
    decoder, offset, modules = json.JSONDecoder(), 0, {}
    while offset < len(raw):
        offset += len(raw[offset:]) - len(raw[offset:].lstrip())
        if offset == len(raw):
            break
        package, offset = decoder.raw_decode(raw, offset)
        module = package.get('Module', {})
        if module and not module.get('Main'):
            modules[module['Path']] = module
    output = ['Third-party notices for the Cloudreve S3 gateway.\n'
              'Generated from modules used by go list -deps ./cmd/cloudreve-s3.\n'
              'Cloudreve upstream GPL source was consulted for API compatibility; '
              'it is not included in the gateway.\n']
    goroot = Path(subprocess.check_output([opts.go, 'env', 'GOROOT'], text=True, encoding='utf-8').strip())
    version = subprocess.check_output([opts.go, 'env', 'GOVERSION'], text=True, encoding='utf-8').strip()
    output.append(f'\nGo standard library and runtime ({version})\n')
    for name in ('LICENSE', 'PATENTS'):
        notice = goroot / name
        if notice.exists():
            output.append(f'\n--- {name} ---\n' + notice.read_text(encoding='utf-8'))
    missing = []
    for name, module in sorted(modules.items()):
        directory = Path(module['Dir'])
        notices = sorted(p for p in directory.iterdir() if p.is_file() and
                         p.name.lower().startswith(('license', 'notice', 'copying')))
        if not notices:
            missing.append(name)
            continue
        output.append(f"\n{'=' * 72}\n{name} {module['Version']}\n")
        for path in notices:
            output.append(f'\n--- {path.name} ---\n' + path.read_text(encoding='utf-8', errors='strict'))
    if missing:
        raise RuntimeError('Modules without root notices: ' + ', '.join(missing))
    target = Path(__file__).resolve().parents[1] / 'third-party' / 'NOTICES.txt'
    target.parent.mkdir(exist_ok=True)
    target.write_text('\n'.join(output), encoding='utf-8')
    print(f'Collected notices for {len(modules)} modules.')


if __name__ == '__main__':
    main()
