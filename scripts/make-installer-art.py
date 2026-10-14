# The Windows installers' images (NSIS header and sidebar, MSI banner and dialog) in the
# Woodland colours, with the app icon and the "ender." logo. Run from the repo root:
#   python scripts/make-installer-art.py
# The app icon ("Spore jar", from the Ender Woodland design) is desktop/app-icon.png (1024 px,
# the same as the iPhone app's); the desktop app's icons come from it: `npx tauri icon app-icon.png`
# in desktop/.
import re
from PIL import Image, ImageDraw, ImageFont

BG, INK, MUTED, DOT, WHITE = "#FAF3E5", "#362A23", "#827563", "#C94B3C", "#FFFFFF"
ICON = Image.open("desktop/app-icon.png").convert("RGB")
OUT = "desktop/src-tauri/installer/"
K = 4  # drawn this many times larger, then scaled down for smooth edges

# The logo's outlines (viewBox 0 -18 326 89); the second subpath of "e" and "d" is a hole.
LETTERS = [
    ("M60 38C60 20 48 8 30 8C12 8 0 21 0 39C0 58 13 70 31 70C43 70 53 66 60 58L44 49C39 55 22 55 20 43H60ZM20 31C22 20 38 20 41 31Z", 0),
    ("M65 69V37C65 18 77 8 95 8C113 8 125 18 125 37V69H105V38C105 30 102 26 95 26C88 26 85 30 85 38V69Z", 0),
    ("M174 -18H194V39C194 58 181 70 163 70C144 70 131 58 131 39C131 21 143 8 161 8C166 8 170 9 174 12ZM151 39C151 47 155 52 163 52C171 52 175 47 175 39C175 31 171 26 163 26C155 26 151 31 151 39Z", 0),
    ("M60 38C60 20 48 8 30 8C12 8 0 21 0 39C0 58 13 70 31 70C43 70 53 66 60 58L44 49C39 55 22 55 20 43H60ZM20 31C22 20 38 20 41 31Z", 200),
    ("M265 69V38C265 17 278 8 299 8V27C289 27 285 31 285 41V69Z", 0),
]

def subpaths(d, dx):
    toks = re.findall(r"[MLHVCZ]|-?\d+(?:\.\d+)?", d)
    out, cur, i, x, y, cmd = [], [], 0, 0.0, 0.0, None
    def n():
        nonlocal i
        i += 1
        return float(toks[i - 1])
    while i < len(toks):
        if toks[i] in "MLHVCZ":
            cmd = toks[i]; i += 1
        if cmd == "Z":
            out.append(cur); cur = []; continue
        if cmd == "M": x, y = n() + dx, n(); cur = [(x, y)]; cmd = "L"
        elif cmd == "L": x, y = n() + dx, n(); cur.append((x, y))
        elif cmd == "H": x = n() + dx; cur.append((x, y))
        elif cmd == "V": y = n(); cur.append((x, y))
        elif cmd == "C":
            a = [n() for _ in range(6)]
            p0, p1, p2, p3 = (x, y), (a[0] + dx, a[1]), (a[2] + dx, a[3]), (a[4] + dx, a[5])
            for s in range(1, 17):
                t = s / 16; u = 1 - t
                cur.append((u**3 * p0[0] + 3 * u * u * t * p1[0] + 3 * u * t * t * p2[0] + t**3 * p3[0],
                            u**3 * p0[1] + 3 * u * u * t * p1[1] + 3 * u * t * t * p2[1] + t**3 * p3[1]))
            x, y = p3
    return out

def logo(img, left, top, height, bg):
    """Draws "ender." with its top-left at (left, top), `height` px tall (all in img's px)."""
    d = ImageDraw.Draw(img); s = height / 89
    tr = lambda p: (left + p[0] * s, top + (p[1] + 18) * s)
    for path, dx in LETTERS:
        for j, sp in enumerate(subpaths(path, dx)):
            d.polygon([tr(p) for p in sp], fill=INK if j == 0 else bg)
    cx, cy = tr((314, 59)); r = 12 * s
    d.ellipse([cx - r, cy - r, cx + r, cy + r], fill=DOT)
    return 326 * s  # width

def canvas(w, h, bg):
    return Image.new("RGB", (w * K, h * K), bg)

def paste_icon(img, cx, cy, size):
    # the whole icon (jar and leaves), with the rounded corners iOS gives it
    icon = ICON.resize((size * K, size * K), Image.LANCZOS)
    mask = Image.new("L", icon.size, 0)
    ImageDraw.Draw(mask).rounded_rectangle([0, 0, icon.width - 1, icon.height - 1], radius=icon.width * 0.225, fill=255)
    img.paste(icon, (int(cx * K - icon.width / 2), int(cy * K - icon.height / 2)), mask)

def text(img, cx, y, lines, size):
    f = ImageFont.truetype("C:/Windows/Fonts/segoeui.ttf", size * K); d = ImageDraw.Draw(img)
    for k, line in enumerate(lines):
        w = d.textlength(line, font=f)
        d.text((cx * K - w / 2, (y + k * size * 1.35) * K), line, font=f, fill=MUTED)

def save(img, w, h, name):
    img.resize((w, h), Image.LANCZOS).save(OUT + name)

def side_panel(img, width, height):
    ImageDraw.Draw(img).rectangle([0, 0, width * K, height * K], fill=BG)
    paste_icon(img, width / 2, 106, 88)
    lw = 326 * 30 / 89
    logo(img, (width / 2 - lw / 2) * K, 178 * K, 30 * K, BG)
    text(img, width / 2, 222, ["Photo & video backup"], 12)

# NSIS header 150x57: icon and logo
img = canvas(150, 57, BG); paste_icon(img, 28, 28.5, 32); logo(img, 52 * K, 18 * K, 21 * K, BG); save(img, 150, 57, "nsis-header.bmp")
# NSIS sidebar 164x314
img = canvas(164, 314, BG); side_panel(img, 164, 314); save(img, 164, 314, "nsis-sidebar.bmp")
# MSI banner 493x58: white, icon on the right
img = canvas(493, 58, WHITE); paste_icon(img, 470, 29, 34); save(img, 493, 58, "wix-banner.bmp")
# MSI dialog 493x312: the sidebar panel on the left, white elsewhere
img = canvas(493, 312, WHITE); side_panel(img, 164, 312); save(img, 493, 312, "wix-dialog.bmp")
print("installer images written")
