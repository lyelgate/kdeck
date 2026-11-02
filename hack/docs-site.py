#!/usr/bin/env python3
"""Builds the documentation site sources in site-docs/ from README.md and
docs/SHORTCUTS.md, so the site never drifts from the README.

Each page takes whole README sections (## headings), in the order listed.
Links to #anchors are rewritten to the page that now holds the heading.
Run: python3 hack/docs-site.py && mkdocs build --strict
"""
import pathlib
import re
import shutil

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "site-docs"
REPO = "https://github.com/lyelgate/kdeck/blob/main/"

PAGES = [
    ("index.md", "kdeck", ["Quick start"]),
    ("install.md", "Install", ["Install", "Shell integration", "Update and settings", "Platforms"]),
    ("shortcuts.md", "Shortcuts and aliases", ["Shortcuts and aliases", "Regex everywhere", "One pod by regex: `dklog abc`", "Port-forward shortcuts: `kdeck pf`"]),
    ("commands.md", "Everyday commands", ["kubectl, built in", "Contexts and namespaces", "Everyday helpers", "Stern-style logs: `kdeck tail` (also `kdeck stern`)", "Troubleshooting", "Many clusters"]),
    ("silent.md", "Silent-failure detection", ["Silent-failure detection (`kdeck silent`)"]),
    ("clouds.md", "Plugins, clouds and Teleport", ["Plugins (krew-compatible)", "Linode LKE", "Clusters on AWS, Google Cloud and Azure", "Teleport plugin (`tsh`)", "Cost"]),
    ("safety.md", "Production safety", ["Production safety", "Security notes"]),
    ("setup.md", "Setup and configuration", ["Setup and diagnostics", "Supported Kubernetes versions", "Configuration reference"]),
    ("development.md", "Development", ["Development", "Limitations and roadmap", "License"]),
]


def slug(title):
    t = re.sub(r"[^\w\- ]", "", title.lower().replace("`", ""))
    return t.replace(" ", "-")


def split(text):
    """Returns the intro and {heading: body} for each ## section, ignoring
    lines inside code fences."""
    intro, sections, current, fence = [], {}, None, False
    for line in text.splitlines(keepends=True):
        if line.startswith("```"):
            fence = not fence
        if not fence and line.startswith("## "):
            current = line[3:].strip()
            sections[current] = []
        if current is None:
            intro.append(line)
        else:
            sections[current].append(line)
    return "".join(intro), {k: "".join(v) for k, v in sections.items()}


def main():
    intro, sections = split((ROOT / "README.md").read_text())
    used = [h for _, _, hs in PAGES for h in hs]
    missing = [h for h in sections if h not in used]
    unknown = [h for h in used if h not in sections]
    if missing or unknown:
        raise SystemExit(f"README sections not on any page: {missing}; listed but not in README: {unknown}")

    where = {slug(h): page for page, _, hs in PAGES for h in hs}
    for h in sections:  # ### headings live on their section's page too
        for sub in re.findall(r"^### (.+)$", sections[h], re.M):
            where[slug(sub)] = next(p for p, _, hs in PAGES if h in hs)

    def fix_links(md):
        md = re.sub(r"\]\(#([\w-]+)\)", lambda m: f"]({where.get(m.group(1), '')}#{m.group(1)})", md)
        md = md.replace("](docs/SHORTCUTS.md)", "](reference.md)")
        md = re.sub(r"\]\((LICENSE|hack/[\w./-]+|\.github/[\w./-]+)\)", lambda m: f"]({REPO}{m.group(1)})", md)
        # GitHub needs \| inside code spans in tables; Python-Markdown already
        # skips pipes in code spans and would show the backslash.
        return re.sub(r"`[^`\n]*`", lambda m: m.group(0).replace("\\|", "|"), md)

    if OUT.exists():
        shutil.rmtree(OUT)
    OUT.mkdir()
    for page, title, heads in PAGES:
        body = []
        if page == "index.md":
            # Intro without the README's own table of contents.
            body.append(re.sub(r"\n(- \[.*\]\(#.*\)\n)+", "\n", intro))
            body.append("## Where next\n\n" + "".join(
                f"- [{t}]({p})\n" for p, t, _ in PAGES[1:4]) + "- [Shortcut reference](reference.md): every shortcut with its full command\n\n")
        elif heads[0] != title:
            body.append(f"# {title}\n\n")
        body += [sections[h] for h in heads]
        if page != "index.md" and heads[0] == title:  # the first section is the page title
            body[0] = "#" + body[0][2:]
        (OUT / page).write_text(fix_links("".join(body)))
    (OUT / "reference.md").write_text((ROOT / "docs/SHORTCUTS.md").read_text().replace("# kdeck shortcut reference", "# Shortcut reference", 1))


if __name__ == "__main__":
    main()
