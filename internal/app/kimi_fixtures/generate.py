"""Generate synthetic Kimi compatibility media; requires Pillow and ffmpeg."""
from pathlib import Path
import math
import subprocess
from PIL import Image, ImageDraw

root = Path(__file__).resolve().parent
Image.new("RGB", (256, 256), (0, 80, 255)).save(root / "blue.png")
flower = Image.new("RGB", (640, 480), (245, 247, 235))
draw = ImageDraw.Draw(flower)
draw.line((320, 220, 320, 460), fill=(38, 125, 47), width=16)
draw.ellipse((220, 330, 322, 375), fill=(49, 155, 63))
draw.ellipse((318, 375, 420, 420), fill=(49, 155, 63))
for i in range(8):
    angle = i * math.pi / 4
    x, y = 320 + math.cos(angle) * 70, 200 + math.sin(angle) * 70
    draw.ellipse((x - 45, y - 45, x + 45, y + 45), fill=(237, 100, 165))
draw.ellipse((275, 155, 365, 245), fill=(251, 199, 36))
flower.save(root / "flower.png")
common = ["-t", "3", "-r", "12", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "+faststart"]
subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-loop", "1", "-i", str(root / "flower.png"), *common, str(root / "flower.mp4")], check=True)
subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c=black:s=320x240:r=12", *common, str(root / "black.mp4")], check=True)
