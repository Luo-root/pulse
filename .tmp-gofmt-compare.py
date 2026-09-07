import subprocess, tempfile, os

files = ["examples/04-flow/main.go","examples/04-flow/main_test.go",
         "examples/internal/demoapp/flowpeak_test.go","examples/internal/demoapp/host.go",
         "loop/events.go","observability/record.go"]
tmpdir = tempfile.mkdtemp()
for ref in ["main","HEAD"]:
    print(f"===== {ref} =====")
    for f in files:
        blob = subprocess.run(['git','show',f'{ref}:{f}'], capture_output=True).stdout
        if blob.startswith(b'fatal'): print(f"  (absent) {f}"); continue
        p = os.path.join(tmpdir, 'x.go')
        with open(p,'wb') as fh: fh.write(blob)
        d = subprocess.run(['gofmt','-d',p], capture_output=True).stdout.decode('utf-8', 'replace')
        lines = [l for l in d.splitlines() if l.startswith(('+','-')) and not l.startswith(('+++','---'))]
        print(f"  {f}: dirty-changes={len(lines)} BOM={blob.startswith(b'\xef\xbb\xbf')}")
        if ref=='HEAD' and lines:
            for l in lines[:14]: print("    "+l)
