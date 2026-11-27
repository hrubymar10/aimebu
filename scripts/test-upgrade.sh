#!/bin/sh
# Build and exercise a released binary against a candidate on disposable data.
set -eu
usage() { echo 'usage: scripts/test-upgrade.sh <old-tag> [<new-ref>] | --from-backup <archive> [<new-ref>]' >&2; exit 2; }
[ "$#" -ge 1 ] && [ "$#" -le 3 ] || usage
mode=upgrade
if [ "$1" = --from-backup ]; then
    [ "$#" -ge 2 ] || usage
    mode=backup
    source=$2
    ref=${3:-HEAD}
    [ -f "$source" ] || { echo 'backup archive not found' >&2; exit 2; }
    source=$(cd "$(dirname "$source")" && pwd -P)/$(basename "$source")
else
    source=$1
    ref=${2:-HEAD}
    git rev-parse -q --verify "refs/tags/$source^{commit}" >/dev/null || { echo 'old tag not found' >&2; exit 2; }
fi
root=$(git rev-parse --show-toplevel)
git -C "$root" rev-parse -q --verify "$ref^{commit}" >/dev/null || { echo 'new ref not found' >&2; exit 2; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aimebu-upgrade-test.XXXXXXXX")
oldtree=
newtree=
cleanup() {
    if [ -n "$newtree" ]; then git -C "$root" worktree remove --force "$newtree" >/dev/null 2>&1 || true; fi
    if [ -n "$oldtree" ]; then git -C "$root" worktree remove --force "$oldtree" >/dev/null 2>&1 || true; fi
    rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
mkdir -p "$tmp/home" "$tmp/codex" "$tmp/claude" "$tmp/config" "$tmp/scratch"
# Every subprocess inherits these roots, including go build and the smoke test.
host_gopath=${GOPATH:-$(go env GOPATH)}
host_gocache=${GOCACHE:-$(go env GOCACHE)}
host_gomodcache=${GOMODCACHE:-$(go env GOMODCACHE)}
export HOME="$tmp/home" CODEX_HOME="$tmp/codex" CLAUDE_CONFIG_DIR="$tmp/claude"
export AIMEBU_CONFIG_DIR="$tmp/config" AIMEBU_SENTRY_DSN=off
export GOPATH="$host_gopath" GOCACHE="$host_gocache" GOMODCACHE="$host_gomodcache"
export AIMEBU_WEB_BUNDLE=off
newtree=$tmp/new-source
git -C "$root" worktree add --detach "$newtree" "$ref" >/dev/null
(cd "$newtree" && go build -o "$tmp/aimebu-new" ./cmd/aimebu)
echo 'PASS candidate build'
if [ "$mode" = upgrade ]; then
    oldtree=$tmp/old-source
    git -C "$root" worktree add --detach "$oldtree" "$source" >/dev/null
    (cd "$oldtree" && go build -o "$tmp/aimebu-old" ./cmd/aimebu)
    echo 'PASS old tag build'
    sh "$root/scripts/upgrade-smoke.sh" "$tmp/aimebu-old" "$tmp/aimebu-new" "$tmp/scratch"
else
    python3 - "$tmp/aimebu-new" "$source" "$tmp" <<'PY'
import hashlib, json, os, pathlib, socket, sqlite3, subprocess, sys, tarfile, time, urllib.request
binary, archive, scratch = map(pathlib.Path, sys.argv[1:])
root=scratch/'config'
assert root.resolve()!=pathlib.Path.home().joinpath('.aimebu').resolve(), 'refusing real data root'
def rows(db):
    queries={'projects':'SELECT id,name,folder FROM project ORDER BY id',
             'rooms':'SELECT id,project_id,kind,title FROM room ORDER BY id',
             'messages':'SELECT id,room_id,author_kind,body FROM message ORDER BY id',
             'agents':'SELECT id,slug,project_id,model FROM agent ORDER BY id',
             'attachments':'SELECT id,project_id,room_id,sha256,size FROM attachment ORDER BY id'}
    connection=sqlite3.connect(':memory:')
    connection.deserialize(db)
    try: return {name:connection.execute(query).fetchall() for name,query in queries.items()}
    finally: connection.close()
with tarfile.open(archive,'r:gz') as tar:
    manifest=json.load(tar.extractfile('manifest.json'))
    names=[m.name for m in tar.getmembers() if m.isfile()]
    assert 'server/aimebu.sqlite' in names, 'backup has no database'
    assert not manifest.get('missing_attachments'), 'backup has missing attachments'
    attachments=[n for n in names if n.startswith('server/attachments/')]
    original={n:hashlib.sha256(tar.extractfile(n).read()).hexdigest() for n in attachments}
    baseline=rows(tar.extractfile('server/aimebu.sqlite').read())
print('PASS backup manifest and attachment files')
env=os.environ.copy()
with socket.socket() as s:
    s.bind(('127.0.0.1',0)); port=s.getsockname()[1]
env.update(AIMEBU_BIND='127.0.0.1',AIMEBU_PORT=str(port),AIMEBU_USAGES='0',AIMEBU_WARM_IDLE='0')
started=False
try:
    result=subprocess.run([str(binary),'restore',str(archive),'--yes'],env=env,capture_output=True,text=True,timeout=120)
    assert result.returncode==0, result.stderr
    print('PASS restore into isolated data root')
    for name,expected in baseline.items():
        restored=rows((root/'server/aimebu.sqlite').read_bytes())[name]
        assert restored==expected, name+' changed during restore'
        print('PASS restored '+name+' count and content')
    for name,digest in original.items():
        assert hashlib.sha256((root/name).read_bytes()).hexdigest()==digest, name
    print('PASS restored attachment contents')
    result=subprocess.run([str(binary),'server','start'],env=env,capture_output=True,text=True,timeout=120)
    assert result.returncode==0, result.stderr
    started=True
    deadline=time.monotonic()+30
    while True:
        try:
            with urllib.request.urlopen('http://127.0.0.1:'+str(port)+'/api/session',timeout=2) as response:
                assert response.status==200
                break
        except Exception:
            if time.monotonic()>deadline: raise
            time.sleep(.2)
    print('PASS restored server answers')
    result=subprocess.run([str(binary),'doctor','--json'],env=env,capture_output=True,text=True,timeout=60)
    assert result.returncode==0, result.stdout+'\n'+result.stderr
    print('PASS live doctor')
    snapshot=scratch/'restored-snapshot.tar.gz'
    result=subprocess.run([str(binary),'backup','--out',str(snapshot)],env=env,capture_output=True,text=True,timeout=120)
    assert result.returncode==0, result.stdout+'\n'+result.stderr
    with tarfile.open(snapshot,'r:gz') as tar:
        after=rows(tar.extractfile('server/aimebu.sqlite').read())
    for name,expected in baseline.items():
        assert after[name]==expected, name+' changed after server migration'
        print('PASS migrated '+name+' count and content')
finally:
    result=subprocess.run([str(binary),'server','stop','--with-supervisor'],env=env,capture_output=True,text=True,timeout=40)
    if started and result.returncode!=0: raise RuntimeError('server cleanup failed: '+result.stderr)
    print('PASS server cleanup')
PY
fi
