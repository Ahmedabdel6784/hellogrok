#!/usr/bin/env python3
"""Generate the hellogrok app icon and Windows multi-size ICO."""

from __future__ import annotations

from pathlib import Path
import shutil
import subprocess

from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / "assets"
CMD_ICO = ROOT / "cmd" / "hellogrok" / "icon.ico"
CMD_PNG = ROOT / "cmd" / "hellogrok" / "icon.png"
RSRC_VERSION = "v0.10.2"


def make_icon(size: int) -> Image.Image:
    s = max(256, min(size * 8, 2048))
    scale = lambda value: round(value * s)

    background_mask = Image.new("L", (s, s), 0)
    ImageDraw.Draw(background_mask).rounded_rectangle(
        (scale(0.045), scale(0.045), scale(0.955), scale(0.955)),
        radius=scale(0.205),
        fill=255,
    )

    # A restrained vertical lift keeps the large icon from looking flat without
    # introducing details that disappear in the 16 px tray rendering.
    background = Image.new("RGBA", (s, s))
    bd = ImageDraw.Draw(background)
    for y in range(s):
        mix = y / max(1, s - 1)
        value = round(18 - 8 * mix)
        bd.line((0, y, s, y), fill=(value, value + 2, value + 4, 255))
    img = Image.new("RGBA", (s, s), (0, 0, 0, 0))
    img.paste(background, (0, 0), background_mask)

    glyph = Image.new("L", (s, s), 0)
    gd = ImageDraw.Draw(glyph)

    # The left H shares its right stem with the open G. All major strokes use
    # the same optical weight so the monogram remains recognizable at 16 px.
    gd.polygon(
        [
            (scale(0.205), scale(0.285)),
            (scale(0.315), scale(0.205)),
            (scale(0.315), scale(0.455)),
            (scale(0.455), scale(0.455)),
            (scale(0.455), scale(0.350)),
            (scale(0.555), scale(0.295)),
            (scale(0.555), scale(0.555)),
            (scale(0.455), scale(0.605)),
            (scale(0.315), scale(0.605)),
            (scale(0.315), scale(0.735)),
            (scale(0.205), scale(0.815)),
        ],
        fill=255,
    )

    # Open rounded G: the right-side opening prevents the mark from becoming a
    # generic ring or arrow while the inward bar reads as a routing gateway.
    gd.rounded_rectangle(
        (scale(0.445), scale(0.285), scale(0.805), scale(0.760)),
        radius=scale(0.155),
        fill=255,
    )
    gd.rounded_rectangle(
        (scale(0.555), scale(0.400), scale(0.695), scale(0.645)),
        radius=scale(0.055),
        fill=0,
    )
    gd.rectangle(
        (scale(0.660), scale(0.395), scale(0.825), scale(0.520)),
        fill=0,
    )
    gd.rectangle(
        (scale(0.625), scale(0.515), scale(0.805), scale(0.605)),
        fill=255,
    )

    # A narrow diagonal cut is the proxy path. It deliberately stops short of
    # forming an X, keeping the symbol independent from existing brand marks.
    gd.polygon(
        [
            (scale(0.315), scale(0.720)),
            (scale(0.675), scale(0.360)),
            (scale(0.715), scale(0.400)),
            (scale(0.355), scale(0.760)),
        ],
        fill=0,
    )

    if size <= 20:
        glyph = Image.new("L", (s, s), 0)
        gd = ImageDraw.Draw(glyph)
        gd.polygon(
            [
                (scale(0.190), scale(0.285)),
                (scale(0.290), scale(0.220)),
                (scale(0.290), scale(0.455)),
                (scale(0.390), scale(0.455)),
                (scale(0.390), scale(0.285)),
                (scale(0.490), scale(0.220)),
                (scale(0.490), scale(0.745)),
                (scale(0.390), scale(0.805)),
                (scale(0.390), scale(0.575)),
                (scale(0.290), scale(0.575)),
                (scale(0.290), scale(0.745)),
                (scale(0.190), scale(0.805)),
            ],
            fill=255,
        )
        gd.rounded_rectangle(
            (scale(0.500), scale(0.270), scale(0.825), scale(0.750)),
            radius=scale(0.140),
            fill=255,
        )
        gd.rounded_rectangle(
            (scale(0.600), scale(0.385), scale(0.710), scale(0.640)),
            radius=scale(0.045),
            fill=0,
        )
        gd.rectangle(
            (scale(0.690), scale(0.400), scale(0.840), scale(0.515)),
            fill=0,
        )
        gd.rectangle(
            (scale(0.640), scale(0.520), scale(0.825), scale(0.615)),
            fill=255,
        )

    glyph_layer = Image.new("RGBA", (s, s), (247, 244, 236, 255))
    img.paste(glyph_layer, (0, 0), glyph)

    accent = Image.new("RGBA", (s, s), (0, 0, 0, 0))
    if size <= 20:
        accent_points = [
            (scale(0.400), scale(0.790)),
            (scale(0.490), scale(0.595)),
            (scale(0.585), scale(0.595)),
            (scale(0.490), scale(0.790)),
        ]
    else:
        accent_points = [
            (scale(0.380), scale(0.775)),
            (scale(0.460), scale(0.600)),
            (scale(0.540), scale(0.600)),
            (scale(0.475), scale(0.775)),
        ]
    ImageDraw.Draw(accent).polygon(accent_points, fill=(54, 230, 190, 255))
    img = Image.alpha_composite(img, accent)

    return img.resize((size, size), Image.Resampling.LANCZOS)


def main() -> None:
    ASSETS.mkdir(exist_ok=True)
    sizes = [16, 20, 24, 32, 40, 48, 64, 128, 256]
    images = [make_icon(sz) for sz in sizes]
    make_icon(1024).save(ASSETS / "hellogrok.png")
    images[-1].save(CMD_PNG)
    for sz in (16, 32, 64):
        images[sizes.index(sz)].save(ASSETS / f"hellogrok-{sz}.png")
    images[-1].save(
        CMD_ICO,
        format="ICO",
        sizes=[(im.width, im.height) for im in images],
        append_images=images[:-1],
    )
    images[-1].save(
        ASSETS / "hellogrok.ico",
        format="ICO",
        sizes=[(im.width, im.height) for im in images],
        append_images=images[:-1],
    )
    go = shutil.which("go")
    if not go:
        raise RuntimeError("Go is required to regenerate Windows icon resources")
    for arch in ("amd64", "arm64"):
        output = ROOT / "cmd" / "hellogrok" / f"rsrc_windows_{arch}.syso"
        subprocess.run(
            [
                go,
                "run",
                f"github.com/akavel/rsrc@{RSRC_VERSION}",
                "-arch",
                arch,
                "-ico",
                str(CMD_ICO),
                "-o",
                str(output),
            ],
            check=True,
        )
        print("wrote", output)
    print("wrote", CMD_ICO)
    print("wrote", ASSETS)


if __name__ == "__main__":
    main()
