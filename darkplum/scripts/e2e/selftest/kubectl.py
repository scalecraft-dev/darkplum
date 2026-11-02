#!/usr/bin/env python3
"""A stand-in for kubectl for selftest.sh: kubeconfigs are plain text files, cluster
state is JSON under $FAKE_STATE/<install|decoy>/. Not a kubectl emulator; just the
calls the e2e scripts make."""
import json
import os
import re
import sys

args = sys.argv[1:]
kc = ctx = None
rest = []
i = 0
while i < len(args):
    if args[i] == "--kubeconfig":
        kc = args[i + 1]; i += 2
    elif args[i] == "--context":
        ctx = args[i + 1]; i += 2
    elif args[i] == "-n":
        i += 2
    else:
        rest.append(args[i]); i += 1
if kc is None:
    print("fake kubectl: no --kubeconfig (the scripts must always pin one)", file=sys.stderr)
    sys.exit(97)
text = open(kc).read()
which = "decoy" if "decoy" in os.path.basename(os.path.dirname(kc)) + os.path.basename(kc) else "install"
state = os.path.join(os.environ["FAKE_STATE"], which)


def field(name):
    m = re.search(r"^%s: (.*)$" % name, text, re.M)
    return m.group(1) if m else ""


if rest[:2] == ["config", "current-context"]:
    print(field("current-context"))
elif rest[:2] == ["config", "get-contexts"]:
    print("\n".join(field("contexts").split()))
elif rest[:2] == ["config", "use-context"]:
    open(kc, "w").write(re.sub(r"^current-context: .*$", "current-context: " + rest[2], text, flags=re.M))
elif rest[:2] == ["config", "view"]:
    print(field("server") if any("jsonpath" in a for a in rest) else text, end="" if not any("jsonpath" in a for a in rest) else "\n")
elif rest[0] == "get":
    if rest[1] == "secret":
        p = os.path.join(state, "secret-%s.json" % rest[2])
    elif rest[1] == "pods":
        p = os.path.join(state, "pods.json")
    elif rest[1] == "deploy" and len(rest) > 2 and not rest[2].startswith("-"):
        p = os.path.join(state, "deploy-%s.json" % rest[2])
    elif rest[1] == "deploy":
        p = os.path.join(state, "deploys.json")
    else:
        p = os.path.join(state, "objects.json")
    if not os.path.exists(p):
        print('Error from server (NotFound): %s not found' % rest[1:3], file=sys.stderr)
        sys.exit(1)
    out = open(p).read()
    # FAKE_TERMINATING_GETS=N: the first N `get pods` calls also list a pod that is
    # still Terminating (the previous rollout draining); 999 means it never goes.
    if rest[1] == "pods" and os.environ.get("FAKE_TERMINATING_GETS"):
        counter = os.path.join(state, ".podgets")
        n = int(open(counter).read()) if os.path.exists(counter) else 0
        open(counter, "w").write(str(n + 1))
        if n < int(os.environ["FAKE_TERMINATING_GETS"]):
            doc = json.loads(out)
            doc["items"].append({"metadata": {"name": "gw-old", "uid": "u0", "deletionTimestamp": "2026-01-01T00:00:00Z"},
                                 "status": {"containerStatuses": [{"restartCount": 0, "imageID": "sha256:old"}]}})
            out = json.dumps(doc)
    sys.stdout.write(out)
else:
    print("fake kubectl: unsupported: %s" % rest, file=sys.stderr)
    sys.exit(98)
