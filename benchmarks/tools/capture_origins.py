"""Supplement Go's JSON snapshots with actual module-proxy Origin metadata."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import json
from pathlib import Path
import urllib.error
import urllib.request


def objects(text):
    decoder = json.JSONDecoder()
    while text.strip():
        item, consumed = decoder.raw_decode(text.lstrip())
        yield item
        text = text.lstrip()[consumed:]


def escaped(value):
    return ''.join('!' + c.lower() if 'A' <= c <= 'Z' else c for c in value)


def capture(item):
    path, version = item['Path'], item['Version']
    url = 'https://proxy.golang.org/' + escaped(path) + '/@v/' + escaped(version) + '.info'
    record = dict(item, info_url=url)
    try:
        with urllib.request.urlopen(url, timeout=15) as response:
            info = json.load(response)
        if not isinstance(info, dict) or info.get('Version') != version:
            raise ValueError('proxy resolved another version')
        record['proxy_info'] = info
        origin = info.get('Origin')
        record['origin_status'] = 'provided' if isinstance(origin, dict) and origin.get('Hash') else 'not_provided'
    except (OSError, ValueError, urllib.error.URLError) as error:
        record.update(origin_status='fetch_failed', error=str(error))
    return record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('manifests', type=Path, nargs='+')
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    modules = {}
    for file in args.manifests:
        manifest = json.loads(file.read_text(encoding='utf-8'))
        for item in objects(manifest['go_download_json']):
            if item.get('Version') and item.get('Sum'):
                modules[item['Path'], item['Version']] = {field: item[field] for field in ('Path', 'Version', 'Sum', 'GoModSum') if field in item}
    with ThreadPoolExecutor(max_workers=4) as pool:
        results = list(pool.map(capture, [item for _, item in sorted(modules.items())]))
    output = {'captured_at_utc': datetime.now(timezone.utc).isoformat(),
              'method': 'Exact measured versions and checksums come from retained go mod download JSON; Origin is queried separately from the same public module proxy. Missing Origin is explicit, never inferred.',
              'provided': sum(r['origin_status'] == 'provided' for r in results),
              'not_provided': sum(r['origin_status'] == 'not_provided' for r in results),
              'fetch_failed': sum(r['origin_status'] == 'fetch_failed' for r in results),
              'modules': results}
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(output, ensure_ascii=False, indent=2) + '\n', encoding='utf-8', newline='\n')
    print(json.dumps({k: output[k] for k in ('provided', 'not_provided', 'fetch_failed')}))


if __name__ == '__main__':
    main()
