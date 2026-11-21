#!/usr/bin/env python3
"""Render production-model ANSI frames through Freeze SVG and Inkscape PNG.

This is a documentation tool. The gateway itself requires only Go.
"""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import unicodedata
import xml.etree.ElementTree as ET


def run(command):
    subprocess.run(command, check=True)


def cell_width(text):
    # The gallery contains no emoji/ZWJ clusters. Terminal combining marks do
    # not advance; East Asian wide/fullwidth characters occupy two cells.
    if "\u200d" in text or "\ufe0f" in text:
        raise ValueError("This gallery renderer requires grapheme-aware positioning for emoji clusters")
    return sum(0 if unicodedata.combining(c) or unicodedata.category(c) == "Cf"
               else 2 if unicodedata.east_asian_width(c) in {"W", "F"} else 1 for c in text)


def position_terminal_cells(svg, plain, foreground):
    # SVG font fallback flows proportionally, unlike an actual terminal. Pin
    # each glyph to the measured terminal grid without changing text or color.
    # In particular, a fallback Braille glyph must not shift later columns.
    ET.register_namespace("", "http://www.w3.org/2000/svg")
    tree = ET.parse(svg)
    root = tree.getroot()
    # An ANSI span without a color uses the terminal's default ink. Freeze's
    # base preset keeps a dark-terminal default even when only -b changes.
    for group in root:
        if group.tag == "{http://www.w3.org/2000/svg}g" and "font-family" in group.attrib:
            group.set("fill", foreground)
    plain_lines = [line for line in plain.read_text().splitlines() if line.strip()]
    columns = max((cell_width(line) for line in plain_lines), default=0)
    if columns <= 0:
        raise ValueError("frame has no rendered columns; refusing to divide by zero")
    advance = (float(root.attrib["width"]) - 36) / columns
    for text in root.iter("{http://www.w3.org/2000/svg}text"):
        cursor = float(text.attrib["x"].removesuffix("px"))
        for span in text:
            value = span.text or ""
            if not value:
                continue
            positions = []
            for char in value:
                positions.append(f"{cursor:.4f}")
                cursor += advance * cell_width(char)
            span.set("x", " ".join(positions))
    tree.write(svg, encoding="unicode", xml_declaration=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--frames", action="store_true", help="regenerate ANSI/text frames using the real Go model")
    parser.add_argument("--font", type=Path, help="optional local monospace font to embed in SVG")
    parser.add_argument("--font-family", default="DejaVu Sans Mono")
    args = parser.parse_args()
    target = Path(__file__).resolve().parent
    root = target.parent.parent
    freeze = shutil.which("freeze")
    inkscape = shutil.which("inkscape")
    if not freeze or not inkscape:
        parser.error("freeze and inkscape must be available on PATH")
    if args.font and not args.font.is_file():
        parser.error("--font must name an existing font file")
    if args.frames:
        run(["go", "-C", str(root), "test", "./internal/tui", "-run", "^TestRenderDashboardGallery$", "-count=1", "-args", "-dashboard-render-dir=" + str(target)])
    frames = sorted(target.glob("*.ansi"))
    if not frames:
        parser.error("no ANSI frames; pass --frames to generate them")
    for frame in frames:
        background = "#FFFFFF" if "-light-" in frame.stem else "#0D1117"
        foreground = "#24292F" if "-light-" in frame.stem else "#E6EDF3"
        svg, png = frame.with_suffix(".svg"), frame.with_suffix(".png")
        command = [freeze, str(frame), "-c", "base", "-l", "ansi", "-b", background,
                   "--font.family", args.font_family, "--font.size", "18", "--line-height", "1.25",
                   "-p", "18", "-m", "0", "-o", str(svg)]
        if args.font:
            command.extend(["--font.file", str(args.font.resolve())])
        run(command)
        position_terminal_cells(svg, frame.with_suffix(".txt"), foreground)
        run([inkscape, str(svg), "--export-type=png", "--export-filename=" + str(png)])
    files = [p for p in sorted(target.iterdir()) if p.suffix in {".ansi", ".txt", ".svg", ".png"}]
    manifest = {
        "provenance": "Production Bubble Tea Model.View; deterministic synthetic observations, not captured customer traffic.",
        "pipeline": "Freeze v0.2.2 ANSI to SVG; explicit terminal-cell glyph positions; Inkscape SVG to PNG with system Unicode font fallback.",
        "scenes": len(frames),
        "files": [{"name": p.name, "bytes": p.stat().st_size,
                   "sha256": hashlib.sha256(p.read_bytes()).hexdigest()} for p in files],
    }
    target.joinpath("render-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Rendered {len(frames)} production-model scenes in {target}")


if __name__ == "__main__":
    main()
