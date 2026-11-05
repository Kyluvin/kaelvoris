---
name: storyboard-video
description: |
  Multi-shot story video production pipeline (分镜短视频): split a theme into
  shots (拆分), write narration copy (文案), keep character/scene consistency
  (人物/场景), generate keyframe images, animate them into video clips
  (image-to-video), then deliver. Use when users ask for 分镜视频 / 故事短视频 /
  图生视频短片 / 带人物的短视频 / storyboard video / multi-scene story clip.
triggers:
  - "storyboard video"
  - "分镜"
  - "图生视频"
  - "短视频管线"
  - "人物一致性"
  - "multi-shot video"
  - "story video"
---

# Storyboard Video Pipeline (分镜短视频)

Turn a theme/idea into a multi-shot short video: each shot has narration copy,
a consistent character, a consistent scene, a keyframe image, and a video clip
animated from that image.

## Pipeline (5 steps)

### Step 1 — 拆分 (Storyboard)

Split the user's theme into 3–6 shots before generating anything. For each
shot write one line: 镜头号 / 画面描述 / 文案（旁白，一句话）. Use
update_todo_list to track the shots.

### Step 2 — 人物与场景设定 (Consistency anchors)

Generate ONE character portrait + ONE establishing scene image with
generate_image first:

```
generate_image(prompt="<角色正脸定妆照，白底>", image=[], ratio="9:16")
```

- 人物一致性：后续每一镜的 keyframe 都把这张角色定妆照放进 `image` 数组
  （image-to-image 参考图），并在 prompt 里重复角色外貌关键词（发型/服装/年龄）。
- 场景一致性：同一场景的分镜复用同一张场景图作参考；换景只换场景描述词。

### Step 3 — 每镜 keyframe 生成

For each shot, generate its keyframe image reusing the character portrait as
reference:

```
generate_image(prompt="镜头2：<画面描述>，<角色外貌关键词>", image=["<定妆照路径>"], ratio="9:16")
```

Keep the output paths from the tool results (<<<KAELVORIS_MEDIA>>> markers).

### Step 4 — 图生视频 (Animate each shot)

Animate each keyframe into a 4–8s clip with generate_video keyframe mode.
Local image paths are auto-uploaded to a public URL (requires 远程服务 login):

```
generate_video(prompt="<镜头动作描述>", mode="keyframe", first_frame="<镜头1图路径>", seconds=5, aspect_ratio="9:16")
```

- mode=keyframe：first_frame/last_frame 控制起止画面（最常用）。
- mode=reference：多参考图（≤5）保人物/风格一致性；无需首帧控制时用。
- **视频模型未配置（关闭/付费未开通）时自动降级**：多张帧图本地合成循环 GIF
  动图（免费、零网络、免图床上传），结果如实标注「GIF 降级」——向用户说明
  这是帧序列轮播（无镜头运动），配置视频模型后可得真 MP4。
- 上传失败（未连接远程服务）时降级：改用 mode=text 纯文生视频，prompt 里
  写全画面细节。

### Step 5 — 合成与交付

Each clip is already saved to disk and shown in chat. To stitch clips or add
TTS narration:

- 配音：generate_speech 逐镜朗读文案，交付音频文件路径清单。
- 拼接：`ffmpeg -f concat -safe 0 -i list.txt -c copy output.mp4`
  （list.txt 每行 `file '<clip路径>'`；ffmpeg 不存在时直接交付分镜片段清单）。

## Delivery

Report per shot: 画面 → 视频路径 → 文案. If generation is not configured,
tell the user to set it up in 设置 → 模型 (图像生成 / 视频生成 / 语音合成).
