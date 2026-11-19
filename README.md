# ImageTileCutter (大图智能切片与交互裁剪工具)

<div align="center">

![ImageTileCutter Icon](icon.png)

**工业级大图切片与交互裁剪桌面端工具**  
*专为计算机视觉、深度学习数据集处理、显微/遥感高分辨图像与巨幅图片切块制作而生*

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Framework](https://img.shields.io/badge/Framework-MyGo%20v0.9.0-6366F1)](https://github.com/egoist/mygo)
[![Platform](https://img.shields.io/badge/Platform-Windows%20x64-blue)](https://github.com/fanweiya/image-tile-cutter/releases)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

</div>

---

## 📖 项目简介

在计算机视觉检测、生物显微图像分析、航天遥感地图处理以及网页瓦片切分场景中，经常面临数千甚至数万像素级的超大图像，直接送入卷积网络或大模型容易引发显存溢出（OOM），或由于大幅缩放导致关键微小目标特征丢失。

**ImageTileCutter** 是一款基于 Go 与高性能 MyGo 现代化轻量 GUI 框架构建的专业大图切块与交互裁剪工具。提供纯客户端图形界面、实时交互式选区框选、并发切片流水线、掩码成对切分、智能背景/低信息过滤以及基于元数据清单的反向无损拼接全生命周期能力。

---

## ✨ 核心特性

### 1. 批量智能切片 (Batch Slicing)
- **滑动窗口切分 (Sliding Window)**:
  - 自由设定切片目标宽度与高度（如 512×512、1024×1024）。
  - 支持配置横向与纵向重叠步长（Overlap X / Y），有效解决目标物体落在切分缝隙处导致无法检测的问题。
- **等分网格切分 (Grid Split)**:
  - 自定义分割行数与列数（如 4×4、8×8），快速将大图均分为矩阵切片。
- **4 种专业边缘处理策略**:
  - `回溯对齐 (保持尺寸)`：边缘余量不足切片大小时，切框自动向原图内回溯对齐，确保所有输出切片尺寸严格一致且无黑边填充。
  - `填充补全 (固定尺寸)`：边缘余量不足时，以纯色背景进行边缘填充，输出绝对固定分辨率。
  - `丢弃边缘 (忽略余量)`：放弃未满足完整切片宽高的边缘碎块。
  - `保留实际 (不规则尺寸)`：直接裁切剩余像素区域，切片实际尺寸与余量相同。

### 2. 智能内容质量过滤 (Smart Filtering)
- **过滤纯单色/背景切片**：
  - 自动统计切片中最常见颜色占比，当单色比例超过设定的敏感度阈值（如 95%）时自动过滤舍弃，避免数据集产生大量纯黑、纯白无效负样本。
- **过滤低方差平坦切片**：
  - 快速计算切片灰度像素方差，自动剔除无纹理特征的模糊平坦切片。

### 3. 成对掩码同步切图 (Paired Mask Slicing)
- 支持同时指定原图目录与配准掩码（Mask / Label）目录。
- 自动智能关联匹配规则（如 `img_001.jpg` 对应 `img_001_mask.png` 或同名文件）。
- 坐标严格空间对准，支持无损最近邻重采样，保障分割标注数据与训练原图切片严格一一对应。

### 4. 交互式可视化框选裁剪 (Interactive Manual ROI)
- **实时拖拽框选**：在视口内按住鼠标左键即可实时绘制目标切取矩形框。
- **多重高亮渲染**：金色双像素边框与半透明填充叠加层，直观预览目标区域。
- **数值实时双向联动**：提供起点 `X`、`Y` 与尺寸 `W`、`H` 的精细调节输入框。
- **单图裁切与全批套用**：
  - 一键将当前框选区域裁剪导出。
  - 一键将所定选区视作固定视野模板，批量裁切当前文件夹下的所有巨幅图像。

### 5. 离散切片无损反向拼接 (Reconstruction / Stitching)
- 切图过程中自动生成工业级 `manifest.json` 元数据清单，包含原图分辨率、切片行列索引、全局绝对包围盒坐标 `[x, y, w, h]`。
- 提供反向拼合引擎，选择清单文件后一键无损重构并拼回超高分辨率全景原始大图。

### 6. 超大图 (Gigapixel / 亿级像素) 深度优化
- **毫秒级头部元数据解析**：采用 `image.DecodeConfig`，在不到 1 毫秒内瞬间获取超大图像尺寸，界面立即响应。
- **后台异步解码流水线**：大图解码在独立后台 Goroutine 执行，搭配原子序列号自动过滤过期请求，主界面 60 FPS 永不卡顿。
- **金字塔预览缓存代理**：自动构建 2048 像素代理缓存，视口实时缩放重采样耗时 `< 2ms`，支持在数万像素级巨幅图像上流畅实时拖拽框选。
- **默认自适应窗口**：载入任何分辨率大图均自动计算最佳贴合比例，免去手动调整烦恼。
- **内存封顶与多核切片并发**：批量切图阶段单图顺序解码进内存，切片多核并发写盘，彻底根治多路大图并发引发的内存爆炸崩溃。

### 7. 多镜像无感在线更新 (Resilient Auto Updates)
- 集成现代化客户端热更新架构与签名校验机制。
- 内置多组 GitHub 镜像加速通道（`ghfast.top`、`gh-proxy.com`、`ghproxy.net`、`ghproxy.cfd` 等），国内网络环境亦可顺畅更新。
- 系统托盘菜单常驻，支持随时点击「检查更新...」。

---

## 🖥 界面概览

| 功能区域 | 说明 |
| :--- | :--- |
| **顶部目录工具栏** | 一键选择原图目录与输出目录，支持直接拖拽图片文件夹到主窗口，带「打开输出文件夹」直达按钮。 |
| **功能选项卡** | `【 批量智能切图 】`、`【 交互框选裁剪 】`、`【 反向还原拼接 】` 随心切换，带多图上一张/下一张翻页控制。 |
| **左侧交互视口** | 支持 `自适应窗口`、`100%`、`50%`、`25%` 视图缩放；实时切换网格分割线辅助预览；支持鼠标拖拽选框。 |
| **右侧配置控制面板** | 包含切图模式、切片长宽、步长重叠、边缘模式、空白过滤、方差过滤、输出格式（PNG/JPG及质量）等全部参数调节。 |
| **底部执行与进度条** | 预估切图数量计算、实时多线程切图进度显示、任务主动终止控制。 |

---

## 🚀 下载与安装

### 预编译版本下载
前往 [GitHub Releases](https://github.com/fanweiya/image-tile-cutter/releases) 页面下载最新构建的 Windows x64 独立可执行程序包 `ImageTileCutter.exe`。绿色单文件，解压即用，无需配置任何外部运行环境。

---

## 🛠 本地源码构建

### 环境要求
- **Go**: 1.22 或更高版本
- **Windows OS**: 64-bit
- **MyGo CLI** (可选但推荐): `go install github.com/egoist/mygo@latest`
- **rsrc** (用于嵌入 Windows 图标): `go install github.com/akavel/rsrc@latest`

### 构建步骤
1. 克隆代码仓库：
   ```bash
   git clone https://github.com/fanweiya/image-tile-cutter.git
   cd image-tile-cutter
   ```

2. 安装依赖：
   ```bash
   go mod download
   ```

3. 一键编译打包：
   - 使用附带的自动化构建脚本：
     ```cmd
     .\build.bat
     ```
   - 或使用原生 Go 命令编译无控制台黑框版本：
     ```bash
     rsrc -ico icon.ico -o rsrc.syso
     go build -ldflags "-s -w -H windowsgui" -o ImageTileCutter.exe .
     ```

---

## 📄 元数据清单 (manifest.json) 结构

切图完成后会在输出目录自动生成一份标准化 `manifest.json` 文件：

```json
{
  "created_at": "2026-11-19T11:30:00+08:00",
  "source_dir": "C:\\images\\raw",
  "output_dir": "C:\\images\\tiles_output",
  "total_tiles": 320,
  "total_images": 20,
  "tiles": [
    {
      "source_file": "slide_001.tif",
      "tile_file": "slide_001_r0_c0_x0_y0_w512_h512.png",
      "mask_file": "slide_001_mask_r0_c0_x0_y0_w512_h512.png",
      "row": 0,
      "col": 0,
      "x": 0,
      "y": 0,
      "width": 512,
      "height": 512,
      "orig_width": 4096,
      "orig_height": 4096
    }
  ]
}
```

---

## 📜 开源协议

本项目基于 [MIT License](LICENSE) 开源发布。
