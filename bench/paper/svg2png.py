#!/usr/bin/env python3
"""Rasterise the paper's SVG figures for the LaTeX build.

No SVG->PDF converter is installed (no rsvg-convert, inkscape or cairosvg), so
we go through macOS Quick Look, which pads its thumbnail to a square. Cropping
that padding back off is the whole reason this script exists: without it every
figure carries a block of whitespace taller than the plot.
"""
import io, os, re, subprocess, sys, tempfile
from PIL import Image, ImageChops

SIZE = 2400          # first thumbnail attempt; reduced on clipping
MARGIN = 8           # px of white kept around the content
MIN_SIZE = 700       # below this we would rather fail than ship a blurry figure


def crop_white(im, margin=MARGIN):
    rgb = im.convert("RGB")
    bg = Image.new("RGB", rgb.size, (255, 255, 255))
    bbox = ImageChops.difference(rgb, bg).getbbox()
    if not bbox:
        return im, None
    l, t, r, b = bbox
    return im.crop((max(l - margin, 0), max(t - margin, 0),
                    min(r + margin, im.width), min(b + margin, im.height))), bbox


def _square_copy(src, d):
    """Return a path to `src` re-framed on a square viewBox, content centred.

    Quick Look renders a square SVG faithfully and silently truncates a wide
    one -- measured directly: an identical drawing on a 900x900 canvas comes
    back whole, on 900x442 it loses its right-hand edge and most of its height.
    Only the outer svg element's width/height/viewBox are rewritten, so the
    drawing itself is untouched and the committed SVG is never modified. The
    padding this adds is removed again by crop_white().
    """
    text = io.open(src, encoding="utf-8").read()
    m = re.search(r'<svg\b[^>]*?viewBox="0 0 ([\d.]+) ([\d.]+)"[^>]*>', text)
    if not m:
        return src
    w, h = float(m.group(1)), float(m.group(2))
    side = max(w, h)
    if abs(w - h) < 1:
        return src
    head = m.group(0)
    new_head = re.sub(r'width="[\d.]+"', 'width="%g"' % side, head)
    new_head = re.sub(r'height="[\d.]+"', 'height="%g"' % side, new_head)
    new_head = new_head.replace('viewBox="0 0 %g %g"' % (w, h),
                                'viewBox="%g %g %g %g"'
                                % (-(side - w) / 2, -(side - h) / 2, side, side))
    out = os.path.join(d, "square-" + os.path.basename(src))
    io.open(out, "w", encoding="utf-8").write(text.replace(head, new_head, 1))
    return out


def _render(src, size, d):
    subprocess.run(["qlmanage", "-t", "-s", str(size), "-o", d, src],
                   capture_output=True, check=False)
    made = [f for f in os.listdir(d)
            if f.endswith(".png") and f.startswith(os.path.basename(src))]
    if not made:
        return None
    return Image.open(os.path.join(d, made[0])).copy()


def convert(src, dst):
    """Rasterise `src` whole, then trim the padding back off."""
    with tempfile.TemporaryDirectory() as d:
        im = _render(_square_copy(src, d), SIZE, d)
        if im is None:
            raise SystemExit("qlmanage produced nothing for %s" % src)
        rgb = im.convert("RGB")
        bbox = ImageChops.difference(
            rgb, Image.new("RGB", rgb.size, (255, 255, 255))).getbbox()
        if bbox is None:
            raise SystemExit("%s rendered blank" % src)
        l, t, r, b = bbox
        if r >= im.width or b >= im.height or l <= 0 or t <= 0:
            raise SystemExit(
                "%s still renders to the canvas edge (%s in %s) -- it would be "
                "clipped, refusing to write a truncated figure" % (src, bbox, im.size))
        out = im.crop((l - MARGIN, t - MARGIN, r + MARGIN, b + MARGIN))
        out.save(dst, "PNG")
        return out.size, SIZE


if __name__ == "__main__":
    for src in sys.argv[1:]:
        if src.endswith(".png"):
            continue
        dst = os.path.splitext(src)[0] + ".png"
        (w, h), used = convert(src, dst)
        note = "" if used == SIZE else "  (reduced to -s %d to avoid clipping)" % used
        print("%-44s -> %s  %dx%d%s" % (os.path.basename(src),
                                        os.path.basename(dst), w, h, note))
