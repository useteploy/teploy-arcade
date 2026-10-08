#!/usr/bin/env python3
"""Refuse reused releases and tags that no longer identify this candidate."""
import json
import os
import subprocess
import urllib.error
import urllib.parse
import urllib.request


def check_identity(tag, expected, run=subprocess.check_output):
    ref = 'refs/tags/' + tag
    lines = run(['git', 'ls-remote', 'origin', ref, ref + '^{}'], text=True).splitlines()
    refs = dict((line.split()[1], line.split()[0]) for line in lines)
    actual = refs.get(ref + '^{}', refs.get(ref))
    if actual != expected:
        raise RuntimeError('Release tag does not identify the verified candidate; use an immutable fresh tag')


def check_unpublished(repository, tag, token, opener=urllib.request.urlopen):
    request = urllib.request.Request(
        f'https://api.github.com/repos/{repository}/releases/tags/{urllib.parse.quote(tag, safe="")}',
        headers={'Authorization': 'Bearer ' + token, 'Accept': 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28'},
    )
    try:
        with opener(request, timeout=30) as response:
            json.load(response)
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return
        raise RuntimeError('Cannot verify release availability') from error
    raise RuntimeError('This version already has a release; publish a fresh version instead of reusing it')


if __name__ == '__main__':
    check_identity(os.environ['GITHUB_REF_NAME'], os.environ['GITHUB_SHA'])
    if '--identity-only' not in __import__('sys').argv:
        check_unpublished(os.environ['GITHUB_REPOSITORY'], os.environ['GITHUB_REF_NAME'], os.environ['GH_TOKEN'])
