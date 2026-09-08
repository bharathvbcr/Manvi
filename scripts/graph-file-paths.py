#!/usr/bin/env python3
"""Print unique file-node paths from a devmap code-graph JSON artifact."""

import json
import sys


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} CODE_GRAPH", file=sys.stderr)
        return 2
    with open(sys.argv[1], encoding="utf-8") as source:
        graph = json.load(source)
    if not isinstance(graph, dict) or not isinstance(graph.get("nodes"), list):
        raise ValueError("code graph must be an object with a nodes array")
    paths = {
        node.get("path")
        for node in graph["nodes"]
        if isinstance(node, dict)
        and node.get("kind") == "file"
        and isinstance(node.get("path"), str)
        and node["path"]
    }
    print("\n".join(sorted(paths)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
