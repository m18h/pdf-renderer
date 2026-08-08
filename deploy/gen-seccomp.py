#!/usr/bin/env python3
"""Generate chrome-seccomp.json from Docker's current default seccomp profile.

Chromium's namespace sandbox needs clone(CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNS),
which Docker's default profile denies for containers without CAP_SYS_ADMIN. That
is why upstream images resort to --no-sandbox.

Rather than ship a hand-written allowlist (the widely-copied jfrazelle chrome.json
is from 2016 and no longer boots under modern runc, which needs statx/STATX_MNT_ID
during container init), this derives a profile from Docker's maintained default
and lifts exactly three restrictions:

  1. clone   - drop the argument filter that forbids namespace flags
  2. clone3  - drop the explicit SCMP_ACT_ERRNO block
  3. unshare - allow, for the sandbox's second-stage namespace setup

Everything else in Docker's default profile is left untouched, so the profile
keeps up with upstream as it changes.

Usage: python3 deploy/gen-seccomp.py > deploy/chrome-seccomp.json
"""

import json
import sys
import urllib.request

SOURCE = "https://raw.githubusercontent.com/moby/profiles/main/seccomp/default.json"

# The mask Docker uses to forbid namespace flags in clone's first argument:
# CLONE_NEWNS|CLONE_NEWUTS|CLONE_NEWIPC|CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNET|CLONE_NEWCGROUP
NAMESPACE_FLAG_MASK = 2114060288


def main() -> int:
    with urllib.request.urlopen(SOURCE) as resp:
        profile = json.load(resp)

    kept = []
    relaxed_clone = False
    dropped_clone3_deny = False

    for block in profile.get("syscalls", []):
        names = block.get("names", [])
        action = block.get("action")

        # 2. Drop the block that denies clone3 without CAP_SYS_ADMIN.
        if action == "SCMP_ACT_ERRNO" and names == ["clone3"]:
            dropped_clone3_deny = True
            continue

        # 1. Drop the namespace-flag filter on clone.
        if action == "SCMP_ACT_ALLOW" and names == ["clone"]:
            args = block.get("args") or []
            if any(a.get("value") == NAMESPACE_FLAG_MASK for a in args):
                block = dict(block)
                block.pop("args", None)
                relaxed_clone = True

        kept.append(block)

    # 3. Allow the remaining syscalls the sandbox needs.
    kept.append({
        "names": ["clone3", "unshare"],
        "action": "SCMP_ACT_ALLOW",
        "args": [],
        "comment": "Chromium namespace sandbox",
        "includes": {},
        "excludes": {},
    })

    if not relaxed_clone:
        print("warning: clone argument filter not found; upstream profile may have "
              "changed shape", file=sys.stderr)
    if not dropped_clone3_deny:
        print("warning: clone3 deny block not found; upstream profile may have "
              "changed shape", file=sys.stderr)

    profile["syscalls"] = kept
    json.dump(profile, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
