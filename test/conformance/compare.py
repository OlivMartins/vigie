"""Compare two runs of the MCP conformance suite: directly against the
reference server, and through Vigie. Every scenario that passes directly must
pass through Vigie.

Usage: python3 compare.py DIRECT.txt VIGIE.txt
"""

import re
import sys

LINE = re.compile(r"^\s*([✓✗])\s+([\w.-]+)(?: \((\w[\w-]*)\))?(?::\s+(\d+) passed, (\d+) failed)?")


def scenarios(path):
    results = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            m = LINE.match(line)
            if m and m.group(4) is not None:
                results[m.group(2)] = (m.group(1) == "✓", int(m.group(4)), int(m.group(5)))
    return results


def main():
    direct, vigie = scenarios(sys.argv[1]), scenarios(sys.argv[2])
    if not direct:
        sys.exit(f"no scenario found in {sys.argv[1]}")
    worse, better = [], []
    for name, (ok, passed, failed) in sorted(direct.items()):
        v = vigie.get(name)
        if v is None:
            worse.append(f"{name}: not run through Vigie")
        elif ok and not v[0]:
            worse.append(f"{name}: passes directly ({passed} checks), fails through Vigie ({v[2]} failed)")
        elif not ok and v[0]:
            better.append(f"{name}: fails directly ({failed} failed), passes through Vigie ({v[1]} checks)")
    print(f"{len(direct)} scenarios, {sum(1 for r in direct.values() if r[0])} passing directly, "
          f"{sum(1 for r in vigie.values() if r[0])} through Vigie")
    for line in better:
        print("  better through Vigie:", line)
    for line in worse:
        print("  WORSE through Vigie:", line)
    sys.exit(1 if worse else 0)


if __name__ == "__main__":
    main()
