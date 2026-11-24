#!/usr/bin/env python3

import argparse
import yaml

parser = argparse.ArgumentParser()
parser.add_argument("manifests", nargs="+")
parser.add_argument("--exact-role", action="append", default=[],
                    help="Append the operator's own RBAC rules without expanding their scope")
args = parser.parse_args()

roles = {}
nrurls = {}
for f in args.manifests:
    with open(f, "r") as  s:
        docs = yaml.load_all(s, Loader=yaml.Loader)

        for doc in docs:
            if doc and doc.get('kind') == 'ClusterRole':
                for rule in doc.get('rules', []):
                    if rule.get('nonResourceURLs'):
                        for r in rule.get('nonResourceURLs', []):
                            verbs = list(set(nrurls.get(r, []) + rule.get('verbs')))
                            nrurls[r] = verbs
                    else:
                        for apig in rule.get('apiGroups', []):
                            for res in rule.get('resources', []):
                                a = roles.get(apig, {})
                                verbs = list(set(a.get(res, []) + rule.get('verbs', [])))
                                verbs.sort()
                                # 'patch' is required by apply.ApplyObject.
                                if 'patch' not in verbs:
                                    verbs.append('patch')
                                a[res] = verbs
                                roles[apig] = a

out = {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRole', 'metadata': {'name': 'antrea-operator'}, 'rules': []}
for apig in sorted(roles.keys()):
    for r, v in sorted(roles[apig].items()):
        out['rules'].append({'apiGroups': [apig], 'resources': [r], 'verbs': v})

for r, v in sorted(nrurls.items()):
    out['rules'].append({'nonResourceURLs': [r], 'verbs': v})
for f in args.exact_role:
    with open(f) as source:
        role = yaml.safe_load(source)
        if role.get("kind") != "ClusterRole":
            raise ValueError("expected a ClusterRole in " + f)
        out["rules"].extend(role["rules"])

print(yaml.dump(out))

exit(0)
