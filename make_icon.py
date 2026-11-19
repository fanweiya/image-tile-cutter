import os
from PIL import Image, ImageDraw

def create_app_icon():
    size = 512
    img = Image.new('RGBA', (size, size), (0, 0, 0, 0))
    draw = ImageDraw.Draw(img)

    margin = 32
    r = 96
    bg_box = [margin, margin, size - margin, size - margin]
    
    # 渐变底色
    for y in range(margin, size - margin):
        t = (y - margin) / float(size - 2 * margin)
        # 深色现代蓝黑渐变
        cr = int(20 + 15 * t)
        cg = int(28 + 25 * t)
        cb = int(45 + 35 * t)
        draw.line([(margin, y), (size - margin, y)], fill=(cr, cg, cb, 255))

    # 圆角遮罩裁剪底色
    mask = Image.new('L', (size, size), 0)
    mask_draw = ImageDraw.Draw(mask)
    mask_draw.rounded_rectangle(bg_box, radius=r, fill=255)
    
    final_bg = Image.new('RGBA', (size, size), (0, 0, 0, 0))
    final_bg.paste(img, (0, 0), mask)
    draw = ImageDraw.Draw(final_bg)

    # 绘制微光圆角外边框
    draw.rounded_rectangle(bg_box, radius=r, outline=(72, 186, 220, 200), width=6)

    # 绘制 2x2 切块方格矩阵
    grid_start = 120
    grid_end = 392
    grid_size = grid_end - grid_start
    tile_gap = 14
    tile_w = (grid_size - tile_gap) // 2

    # 四个切块带有不同现代明亮色彩
    tiles = [
        (grid_start, grid_start, (46, 204, 113, 240)),                    # 左上：翠绿
        (grid_start + tile_w + tile_gap, grid_start, (52, 152, 219, 240)), # 右上：科技蓝
        (grid_start, grid_start + tile_w + tile_gap, (241, 196, 15, 240)), # 左下：金黄
        (grid_start + tile_w + tile_gap, grid_start + tile_w + tile_gap, (155, 89, 182, 240)), # 右下：浅紫
    ]

    for tx, ty, color in tiles:
        tbox = [tx, ty, tx + tile_w, ty + tile_w]
        draw.rounded_rectangle(tbox, radius=20, fill=color, outline=(255, 255, 255, 180), width=4)

    # 绘制裁切参考虚线与直角角标 (Crop Handles)
    crop_margin = 92
    cw = 36
    c_thick = 7
    c_color = (255, 255, 255, 250)

    # 左上角标
    draw.line([(crop_margin, crop_margin), (crop_margin + cw, crop_margin)], fill=c_color, width=c_thick)
    draw.line([(crop_margin, crop_margin), (crop_margin, crop_margin + cw)], fill=c_color, width=c_thick)
    # 右上角标
    draw.line([(size - crop_margin, crop_margin), (size - crop_margin - cw, crop_margin)], fill=c_color, width=c_thick)
    draw.line([(size - crop_margin, crop_margin), (size - crop_margin, crop_margin + cw)], fill=c_color, width=c_thick)
    # 左下角标
    draw.line([(crop_margin, size - crop_margin), (crop_margin + cw, size - crop_margin)], fill=c_color, width=c_thick)
    draw.line([(crop_margin, size - crop_margin), (crop_margin, size - crop_margin - cw)], fill=c_color, width=c_thick)
    # 右下角标
    draw.line([(size - crop_margin, size - crop_margin), (size - crop_margin - cw, size - crop_margin)], fill=c_color, width=c_thick)
    draw.line([(size - crop_margin, size - crop_margin), (size - crop_margin, size - crop_margin - cw)], fill=c_color, width=c_thick)

    # 保存 PNG
    os.makedirs('resources', exist_ok=True)
    png_path = 'resources/icon.png'
    final_bg.save(png_path, 'PNG')
    final_bg.save('icon.png', 'PNG')

    # 生成多分辨率 ICO
    ico_sizes = [(16, 16), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)]
    final_bg.save('icon.ico', format='ICO', sizes=ico_sizes)
    print("Icon generated successfully: icon.png and icon.ico")

if __name__ == '__main__':
    create_app_icon()
