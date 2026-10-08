const {execFileSync} = require('node:child_process');
// Exercise the production guard against exact remote/API response fixtures.
execFileSync('python3', ['-c', `
import importlib.util,io,urllib.error
spec=importlib.util.spec_from_file_location('guard','.github/scripts/release-guard.py')
g=importlib.util.module_from_spec(spec);spec.loader.exec_module(g)
def run(lines): return lambda *args,**kwargs: lines
def refuses(fn):
 try: fn()
 except RuntimeError: return
 raise AssertionError('guard accepted unsafe publication')
g.check_identity('v1','commit',run('commit\\trefs/tags/v1\\n'))
g.check_identity('v1','commit',run('tagobject\\trefs/tags/v1\\ncommit\\trefs/tags/v1^{}\\n'))
refuses(lambda:g.check_identity('v1','commit',run('other\\trefs/tags/v1\\n')))
refuses(lambda:g.check_identity('v1','commit',run('')))
def unavailable(code):
 def open(*args,**kwargs): raise urllib.error.HTTPError('url',code,'test',{},None)
 return open
g.check_unpublished('owner/repo','v1','fixture',unavailable(404))
refuses(lambda:g.check_unpublished('owner/repo','v1','fixture',unavailable(403)))
refuses(lambda:g.check_unpublished('owner/repo','v1','fixture',unavailable(500)))
refuses(lambda:g.check_unpublished('owner/repo','v1','fixture',lambda *a,**k:io.BytesIO(b'{}')))
print('release guard: 8 identity/publication cases pass')
`], {stdio:'inherit'});
