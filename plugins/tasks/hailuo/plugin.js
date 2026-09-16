// 视频时长是所有视频模型共有的计费数量，具体模型的可选分辨率由 profile 限定。
const VIDEO_SECONDS_FIELD = {
  type: "number",
  unit: "second",
  description: { en: "Video generation unit price", zh: "视频生成单价" },
};

const RESOLUTION_DESCRIPTION = { en: "Output video resolution", zh: "输出视频分辨率" };

// H3 再生成需要一个可供表达式区分的操作枚举，字段值是事实而不是价格。
const H3_OPERATION_FIELD = {
  enum: ["generation", "regeneration", "context_ir"],
  enumLabels: {
    generation: { en: "Generation", zh: "普通生成" },
    regeneration: { en: "Regeneration", zh: "视频再生成" },
    context_ir: { en: "H3-Context-IR", zh: "H3-Context-IR" },
  },
  description: { en: "Video operation", zh: "视频操作" },
};

// H3 普通生成与再生成共用时长、分辨率和输入媒体计费维度。
const H3_USAGE_SCHEMA = {
  seconds: VIDEO_SECONDS_FIELD,
  resolution: {
    enum: ["768P", "2K"],
    description: RESOLUTION_DESCRIPTION,
  },
  input_images: {
    type: "number",
    unit: "count",
    unitLabel: { en: "image", zh: "张" },
    description: { en: "Input image unit price", zh: "输入图片单价" },
  },
  input_video_seconds: {
    type: "number",
    unit: "second",
    description: { en: "Input video unit price", zh: "输入视频单价" },
  },
};

// 按操作声明有效的计费字段，避免价格编辑器生成“再生成 768P”或为文本任务按秒定价。
const H3_VIDEO_PRICE_WHEN = [{ field: "operation", values: ["generation", "regeneration"] }];
const H3_BILLING_USAGE_SCHEMA = {
  operation: H3_OPERATION_FIELD,
  resolution: Object.assign({}, H3_USAGE_SCHEMA.resolution, {
    when: [
      { field: "operation", values: ["generation"] },
      { field: "operation", values: ["regeneration"], enum: ["2K"] },
    ],
  }),
  // 显式声明价格列顺序，避免后端 map 序列化将视频单价挤到素材价格之后。
  seconds: Object.assign({}, VIDEO_SECONDS_FIELD, { displayOrder: 10, when: H3_VIDEO_PRICE_WHEN }),
  input_images: Object.assign({}, H3_USAGE_SCHEMA.input_images, { displayOrder: 40, when: H3_VIDEO_PRICE_WHEN }),
  input_video_seconds: Object.assign({}, H3_USAGE_SCHEMA.input_video_seconds, { displayOrder: 50, when: H3_VIDEO_PRICE_WHEN }),
  // 官方 usage 分别报告 prompt_tokens 和 completion_tokens，均按百万 Token 编辑单价。
  prompt_tokens: {
    type: "number",
    unit: "token",
    displayOrder: 20,
    description: { en: "H3-Context-IR input token unit price", zh: "H3-Context-IR 输入 Token 单价" },
    when: [{ field: "operation", values: ["context_ir"] }],
  },
  completion_tokens: {
    type: "number",
    unit: "token",
    displayOrder: 30,
    description: { en: "H3-Context-IR output token unit price", zh: "H3-Context-IR 输出 Token 单价" },
    when: [{ field: "operation", values: ["context_ir"] }],
  },
};

// 2.3/02 和 01 系列保留原有 profile，避免 H3 专用字段污染其他模型的价格编辑器。
const HAILUO_23_USAGE_SCHEMA = {
  seconds: VIDEO_SECONDS_FIELD,
  resolution: {
    enum: ["768P", "1080P"],
    description: RESOLUTION_DESCRIPTION,
  },
};

const HAILUO_02_USAGE_SCHEMA = {
  seconds: VIDEO_SECONDS_FIELD,
  resolution: {
    enum: ["512P", "768P", "1080P"],
    description: RESOLUTION_DESCRIPTION,
  },
};

const HAILUO_01_USAGE_SCHEMA = {
  seconds: VIDEO_SECONDS_FIELD,
};

export const meta = {
  apiVersion: 1,
  key: "hailuo",
  name: "Hailuo Video",
  icon: "Hailuo.Color",
  description: {
    en: "MiniMax Hailuo video generation (text-to-video, image-to-video, H3 multimodal reference, H3-Context-IR, and video regeneration)",
    zh: "MiniMax 海螺视频生成（文生视频、图生视频、H3 多模态参考、H3-Context-IR 和视频再生成）",
  },
  version: "1.3.6",
  author: { name: "QuantumNous" },
  channelTypes: [35],
  models: [
    "MiniMax-H3",
    "MiniMax-Hailuo-2.3",
    "MiniMax-Hailuo-2.3-Fast",
    "MiniMax-Hailuo-02",
    "T2V-01-Director",
    "T2V-01",
    "I2V-01-Director",
    "I2V-01-live",
    "I2V-01",
    "S2V-01",
  ],
  fetchMode: "per_task",
  // 未映射别名使用全部模型维度，具体模型优先使用下方 profile。
  usageSchema: Object.assign({}, H3_USAGE_SCHEMA, {
    resolution: {
      enum: ["512P", "768P", "720P", "1080P", "2K"],
      description: RESOLUTION_DESCRIPTION,
    },
  }),
  usageExamples: [
    { label: "2.3/02 768P 6s", facts: { seconds: 6, resolution: "768P", input_images: 0, input_video_seconds: 0 } },
    { label: "2.3/02 768P 10s", facts: { seconds: 10, resolution: "768P", input_images: 0, input_video_seconds: 0 } },
    { label: "2.3/02 1080P 6s", facts: { seconds: 6, resolution: "1080P", input_images: 0, input_video_seconds: 0 } },
    { label: "02 512P 6s", facts: { seconds: 6, resolution: "512P", input_images: 0, input_video_seconds: 0 } },
    { label: "02 512P 10s", facts: { seconds: 10, resolution: "512P", input_images: 0, input_video_seconds: 0 } },
    { label: "01-series 720P 6s", facts: { seconds: 6, resolution: "720P", input_images: 0, input_video_seconds: 0 } },
    { label: "H3 768P 5s", facts: { seconds: 5, resolution: "768P", input_images: 0, input_video_seconds: 0 } },
    { label: "H3 2K 5s · 9 images", facts: { seconds: 5, resolution: "2K", input_images: 9, input_video_seconds: 0 } },
    { label: "H3 2K 5s · input video", facts: { seconds: 5, resolution: "2K", input_images: 0, input_video_seconds: 15 } },
  ],
  usageProfiles: [
    {
      models: ["MiniMax-H3"],
      schema: H3_BILLING_USAGE_SCHEMA,
      examples: [
        {
          label: "H3 generation 768P 5s",
          facts: {
            seconds: 5,
            resolution: "768P",
            input_images: 0,
            input_video_seconds: 0,
            prompt_tokens: 0,
            completion_tokens: 0,
            operation: "generation",
          },
        },
        {
          label: "H3 generation 2K 5s",
          facts: {
            seconds: 5,
            resolution: "2K",
            input_images: 0,
            input_video_seconds: 0,
            prompt_tokens: 0,
            completion_tokens: 0,
            operation: "generation",
          },
        },
        {
          label: "H3 regeneration 2K 5s",
          facts: {
            seconds: 5,
            resolution: "2K",
            input_images: 0,
            input_video_seconds: 0,
            prompt_tokens: 0,
            completion_tokens: 0,
            operation: "regeneration",
          },
        },
        {
          label: "H3 regeneration 2K 9 images",
          facts: {
            seconds: 5,
            resolution: "2K",
            input_images: 9,
            input_video_seconds: 0,
            prompt_tokens: 0,
            completion_tokens: 0,
            operation: "regeneration",
          },
        },
        {
          label: "H3-Context-IR 5664 input + 3426 output tokens",
          facts: {
            seconds: 0,
            resolution: "768P",
            input_images: 0,
            input_video_seconds: 0,
            prompt_tokens: 5664,
            completion_tokens: 3426,
            operation: "context_ir",
          },
        },
      ],
    },
    {
      models: ["MiniMax-Hailuo-2.3", "MiniMax-Hailuo-2.3-Fast"],
      schema: HAILUO_23_USAGE_SCHEMA,
      examples: [
        { label: "2.3 768P 6s", facts: { seconds: 6, resolution: "768P" } },
        { label: "2.3 768P 10s", facts: { seconds: 10, resolution: "768P" } },
        { label: "2.3 1080P 6s", facts: { seconds: 6, resolution: "1080P" } },
      ],
    },
    {
      models: ["MiniMax-Hailuo-02"],
      schema: HAILUO_02_USAGE_SCHEMA,
      examples: [
        { label: "02 512P 6s", facts: { seconds: 6, resolution: "512P" } },
        { label: "02 512P 10s", facts: { seconds: 10, resolution: "512P" } },
        { label: "02 768P 6s", facts: { seconds: 6, resolution: "768P" } },
        { label: "02 768P 10s", facts: { seconds: 10, resolution: "768P" } },
        { label: "02 1080P 6s", facts: { seconds: 6, resolution: "1080P" } },
      ],
    },
    {
      models: ["T2V-01-Director", "T2V-01", "I2V-01-Director", "I2V-01-live", "I2V-01", "S2V-01"],
      schema: HAILUO_01_USAGE_SCHEMA,
      examples: [{ label: "01-series 720P 6s", facts: { seconds: 6 } }],
    },
  ],
  routes: [
    { method: "POST", path: "/hailuo/v2/video_generation", type: "submit", models: ["MiniMax-H3"], decode: "createH3VideoTask", render: "h3TaskCreated" },
    { method: "GET", path: "/hailuo/v2/query/video_generation/:task_id", type: "query", models: ["MiniMax-H3"], render: "h3TaskStatus" },
    {
      method: "GET",
      path: "/hailuo/v2/query/video_generation",
      type: "dynamic",
      action: "list",
      models: ["MiniMax-H3"],
      decode: "decodeH3TaskList",
      render: "h3TaskList",
    },
    {
      method: "DELETE",
      path: "/hailuo/v2/video_generation/:task_id",
      type: "dynamic",
      action: "delete",
      models: ["MiniMax-H3"],
      decode: "decodeH3TaskDelete",
      render: "h3TaskDeleted",
    },
    { method: "POST", path: "/hailuo/v2/h3_context_ir", type: "submit", models: ["MiniMax-H3"], decode: "createH3ContextIRTask", render: "h3TaskCreated" },
    {
      method: "POST",
      path: "/hailuo/v2/video_regeneration",
      type: "submit",
      models: ["MiniMax-H3"],
      decode: "createH3RegenerationTask",
      render: "h3TaskCreated",
    },
  ],
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};

function trimmed(value) {
  return String(value || "").trim();
}

function isModernHailuo(model) {
  return model === "MiniMax-Hailuo-2.3" || model === "MiniMax-Hailuo-2.3-Fast" || model === "MiniMax-Hailuo-02";
}

function defaultResolution(model) {
  if (model === "MiniMax-Hailuo-2.3" || model === "MiniMax-Hailuo-2.3-Fast" || model === "MiniMax-Hailuo-02") return "768P";
  return "720P";
}

function resolutionFor(size, model) {
  const value = String(size || "");
  if (value.includes("1080")) return "1080P";
  if (value.includes("768")) return "768P";
  if (value.includes("720")) return isModernHailuo(model) ? "768P" : "720P";
  if (value.includes("512")) return "512P";
  return defaultResolution(model);
}

function outboundDuration(req) {
  const n = Number(req && req.duration);
  if (Number.isFinite(n) && n > 0) return n;
  return 6;
}

function outboundResolution(req, model) {
  if (req && req.resolution) return resolutionFor(req.resolution, model);
  const metadata = (req && req.metadata) || {};
  if (metadata.resolution) return resolutionFor(metadata.resolution, model);
  if (req && req.size) return resolutionFor(req.size, model);
  return defaultResolution(model);
}

function hasHailuoImage(req, hasInputReferenceFile) {
  if (hasInputReferenceFile) return true;
  const metadata = (req && req.metadata) || {};
  return Boolean(
    trimmed(req && req.input_reference) ||
    trimmed(req && req.image) ||
    (Array.isArray(req && req.images) && req.images.length) ||
    metadata.first_frame_image ||
    metadata.last_frame_image ||
    metadata.subject_reference
  );
}

const H3_MODEL = "MiniMax-H3";
const H3_MIN_DURATION = 4;
const H3_MAX_DURATION = 15;
const H3_DEFAULT_DURATION = 5;
const H3_MAX_FRAME_IMAGES = 2;
const H3_MAX_REFERENCE_IMAGES = 9;
const H3_MAX_REFERENCE_VIDEOS = 3;
const H3_MAX_REFERENCE_AUDIOS = 3;
const H3_MAX_INPUT_VIDEO_SECONDS = 15;
const H3_RATIOS = ["adaptive", "21:9", "16:9", "4:3", "1:1", "3:4", "9:16"];
const H3_REGENERATION_ACTION = "regeneration";
const H3_CONTEXT_IR_ACTION = "context_ir";
const H3_GENERATION_OPERATION = "generation";
const H3_REGENERATION_OPERATION = "regeneration";
const H3_CONTEXT_IR_OPERATION = "context_ir";
// 官方 task_type 与内部计费 operation 使用不同名称，不能直接互换。
const H3_CONTEXT_IR_TASK_TYPE = "h3_context_ir";
const H3_REGENERATION_RESOLUTION = "2K";
// 输出增强提示词无法事先分词，沿用原有固定预留量作为输出估算。
const H3_CONTEXT_IR_ESTIMATED_OUTPUT_TOKENS = 10000;
const H3_CONTEXT_IR_MAX_TOKENS = 2147483647;

// MiniMax-H3 speaks the /v2 video generation contract: a multimodal `content`
// array instead of flat frame fields, an explicit `ratio`, 768P/2K resolutions,
// a task id path parameter on query, and a `{task: {...}}` query envelope.
function isH3(model) {
  return model === H3_MODEL;
}

function h3Duration(req) {
  const raw = req.duration;
  if (raw === undefined || raw === null || raw === "") return H3_DEFAULT_DURATION;
  const seconds = Number(raw);
  if (!Number.isInteger(seconds) || seconds < H3_MIN_DURATION || seconds > H3_MAX_DURATION) {
    throw new Error(H3_MODEL + " duration must be an integer between " + H3_MIN_DURATION + " and " + H3_MAX_DURATION + " seconds");
  }
  return seconds;
}

function h3Resolution(req) {
  const metadata = req.metadata || {};
  const raw = trimmed(metadata.resolution) || trimmed(req.resolution) || trimmed(req.size);
  if (!raw) return "768P";
  const value = raw.toUpperCase();
  if (value.includes("2K")) return "2K";
  if (value.includes("768")) return "768P";
  throw new Error(H3_MODEL + " resolution must be 768P or 2K");
}

function h3MediaItem(type, url, role) {
  const item = { type: type, role: role };
  item[type] = { url: url };
  return item;
}

// Accepts a single value or an array; file placeholders stay objects and are
// resolved by the host after the body is built.
function h3MediaList(source, key) {
  const raw = source[key];
  if (raw === undefined || raw === null) return [];
  const values = Array.isArray(raw) ? raw : [raw];
  return values.filter(function (value) {
    return value && typeof value === "object" ? true : Boolean(trimmed(value));
  });
}

function h3FrameImages(req) {
  const metadata = req.metadata || {};
  const images = h3MediaList(req, "images");
  if (images.length > H3_MAX_FRAME_IMAGES) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_FRAME_IMAGES + " frame images");
  const frames = [];
  if (metadata.first_frame_image) frames.push(h3MediaItem("image_url", metadata.first_frame_image, "first_frame"));
  if (metadata.last_frame_image) frames.push(h3MediaItem("image_url", metadata.last_frame_image, "last_frame"));
  if (frames.length) return frames;
  return images.map(function (url, index) {
    return h3MediaItem("image_url", url, index === 0 ? "first_frame" : "last_frame");
  });
}

function validateH3Content(items) {
  let hasText = false;
  let hasFrame = false;
  let hasReference = false;
  let firstFrames = 0;
  let lastFrames = 0;
  let referenceImages = 0;
  let referenceVideos = 0;
  let referenceAudios = 0;
  let inputImages = 0;
  for (const item of items) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue;
    const role = trimmed(item.role);
    if (item.type === "text" && trimmed(item.text)) {
      hasText = true;
      continue;
    }
    if (item.type === "image_url") {
      inputImages += 1;
      if (!role || role === "first_frame") {
        firstFrames += 1;
        hasFrame = true;
      } else if (role === "last_frame") {
        lastFrames += 1;
        hasFrame = true;
      } else if (role === "middle_frame") {
        hasFrame = true;
      } else if (role === "reference_image") {
        referenceImages += 1;
        hasReference = true;
      }
      continue;
    }
    if (item.type === "video_url") {
      referenceVideos += 1;
      hasReference = true;
      continue;
    }
    if (item.type === "audio_url") {
      referenceAudios += 1;
      hasReference = true;
    }
  }
  if (!hasText) throw new Error(H3_MODEL + " requires a non-empty text item");
  if (firstFrames > 1) throw new Error(H3_MODEL + " accepts at most one first_frame image");
  if (lastFrames > 1) throw new Error(H3_MODEL + " accepts at most one last_frame image");
  if (referenceImages > H3_MAX_REFERENCE_IMAGES) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_IMAGES + " reference images");
  if (inputImages > H3_MAX_REFERENCE_IMAGES) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_IMAGES + " input images");
  if (referenceVideos > H3_MAX_REFERENCE_VIDEOS) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_VIDEOS + " reference videos");
  if (referenceAudios > H3_MAX_REFERENCE_AUDIOS) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_AUDIOS + " reference audios");
  if (hasFrame && hasReference) throw new Error(H3_MODEL + " cannot mix frame images with reference media");
  return items;
}

// metadata.content is the full multimodal passthrough; otherwise the content
// array is assembled from prompt, frame images, and reference media.
// 读取 H3 的完整 content，兼容 OpenAI Video 顶层字段和历史 metadata 字段。
function h3RequestContent(req) {
  const metadata = (req && req.metadata) || {};
  if (req && Object.prototype.hasOwnProperty.call(req, "content")) return req.content;
  if (Object.prototype.hasOwnProperty.call(metadata, "content")) return metadata.content;
  return undefined;
}

// 统一校验再生成使用的源任务标识，后续只接受网关公开的任务 ID。
function h3SourceTaskID(req) {
  const value = req && req.source_task_id;
  if (value === undefined || value === null) return "";
  if (typeof value !== "string" || !trimmed(value)) throw new Error(H3_MODEL + " source_task_id must be a non-empty string");
  return trimmed(value);
}

function h3MediaURL(item, key) {
  const media = item && item[key];
  if (typeof media === "string") return trimmed(media);
  if (media && typeof media === "object" && !Array.isArray(media)) return trimmed(media.url);
  return "";
}

function h3HasBaseVideo(req) {
  const content = h3RequestContent(req);
  if (!Array.isArray(content)) return false;
  return content.some(function (item) {
    return item && typeof item === "object" && item.type === "video_url" && trimmed(item.role) === "base_video";
  });
}

// 再生成允许保留原始输入，同时把 base_video 从普通参考视频计数中排除。
function validateH3RegenerationContent(items) {
  if (!Array.isArray(items)) throw new Error(H3_MODEL + " regeneration content must be an array");
  let hasText = false;
  let hasFrame = false;
  let hasReference = false;
  let baseVideos = 0;
  let firstFrames = 0;
  let lastFrames = 0;
  let referenceImages = 0;
  let referenceVideos = 0;
  let referenceAudios = 0;
  let inputImages = 0;
  for (const item of items) {
    if (!item || typeof item !== "object" || Array.isArray(item)) throw new Error(H3_MODEL + " regeneration content items must be objects");
    const type = trimmed(item.type);
    const role = trimmed(item.role);
    if (type === "text") {
      if (trimmed(item.text)) hasText = true;
      continue;
    }
    if (type === "image_url") {
      if (!h3MediaURL(item, "image_url")) throw new Error(H3_MODEL + " image_url must include a URL");
      inputImages += 1;
      if (!role || role === "first_frame") {
        firstFrames += 1;
        hasFrame = true;
      } else if (role === "last_frame") {
        lastFrames += 1;
        hasFrame = true;
      } else if (role === "middle_frame") {
        hasFrame = true;
      } else if (role === "reference_image") {
        referenceImages += 1;
        hasReference = true;
      } else {
        throw new Error(H3_MODEL + " image role is invalid");
      }
      continue;
    }
    if (type === "video_url") {
      if (!h3MediaURL(item, "video_url")) throw new Error(H3_MODEL + " video_url must include a URL");
      if (role === "base_video") {
        baseVideos += 1;
      } else {
        if (role && role !== "reference_video") throw new Error(H3_MODEL + " video role is invalid");
        referenceVideos += 1;
        hasReference = true;
      }
      continue;
    }
    if (type === "audio_url") {
      if (!h3MediaURL(item, "audio_url")) throw new Error(H3_MODEL + " audio_url must include a URL");
      if (role && role !== "reference_audio") throw new Error(H3_MODEL + " audio role is invalid");
      referenceAudios += 1;
      hasReference = true;
      continue;
    }
    throw new Error(H3_MODEL + " regeneration content type is invalid");
  }
  if (!hasText) throw new Error(H3_MODEL + " regeneration requires a non-empty text item");
  if (baseVideos !== 1) throw new Error(H3_MODEL + " regeneration requires exactly one base_video item");
  if (firstFrames > 1) throw new Error(H3_MODEL + " accepts at most one first_frame image");
  if (lastFrames > 1) throw new Error(H3_MODEL + " accepts at most one last_frame image");
  if (referenceImages > H3_MAX_REFERENCE_IMAGES || inputImages > H3_MAX_REFERENCE_IMAGES)
    throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_IMAGES + " images");
  if (referenceVideos > H3_MAX_REFERENCE_VIDEOS) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_VIDEOS + " reference videos");
  if (referenceAudios > H3_MAX_REFERENCE_AUDIOS) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_AUDIOS + " reference audios");
  if (hasFrame && hasReference) throw new Error(H3_MODEL + " cannot mix frame images with reference media");
  return items;
}

// 官方再生成要求 content 保留源视频生成时的最终输入，缺少文本时使用兼容层的 prompt。
function h3RegenerationContent(req, fallbackPrompt) {
  const raw = h3RequestContent(req);
  if (!Array.isArray(raw)) throw new Error(H3_MODEL + " regeneration requires content with a base_video item");
  const items = raw.slice();
  const hasText = items.some(function (item) {
    return item && item.type === "text" && trimmed(item.text);
  });
  const prompt = trimmed(req && req.prompt) || trimmed(fallbackPrompt);
  if (!hasText && prompt) items.unshift({ type: "text", text: prompt });
  return validateH3RegenerationContent(items);
}

function h3RegenerationResolution(req) {
  const metadata = (req && req.metadata) || {};
  const raw = req && req.resolution !== undefined ? req.resolution : req && req.size !== undefined ? req.size : metadata.resolution;
  if (raw === undefined || raw === null || raw === "") return H3_REGENERATION_RESOLUTION;
  if (trimmed(raw).toUpperCase() !== H3_REGENERATION_RESOLUTION) throw new Error(H3_MODEL + " regeneration resolution must be 2K");
  return H3_REGENERATION_RESOLUTION;
}

function h3RegenerationRequested(req) {
  if (req && Object.prototype.hasOwnProperty.call(req, "source_task_id")) return true;
  return h3HasBaseVideo(req);
}

// 源任务模式不允许再携带普通生成输入，避免旧调用路径静默丢弃字段。
function h3HasGenerationContent(req) {
  return (
    h3RequestContent(req) !== undefined ||
    req.input !== undefined ||
    req.images !== undefined ||
    req.image !== undefined ||
    req.input_reference !== undefined ||
    Boolean(trimmed(req.prompt))
  );
}

function h3RegenerationRequestedDuration(req) {
  const raw = req && req.duration !== undefined ? req.duration : req && req.seconds;
  if (raw === undefined || raw === null || raw === "") return H3_MAX_DURATION;
  const seconds = Number(raw);
  if (!Number.isInteger(seconds) || seconds < H3_MIN_DURATION || seconds > H3_MAX_DURATION) {
    throw new Error(H3_MODEL + " regeneration duration estimate must be an integer between " + H3_MIN_DURATION + " and " + H3_MAX_DURATION + " seconds");
  }
  return seconds;
}

// 任务数据可能来自初始提交、轮询响应或网关包装层，逐层定位上游 H3 task 对象。
function h3TaskPayload(value) {
  let current = value;
  for (let depth = 0; depth < 4; depth += 1) {
    if (!current || typeof current !== "object" || Array.isArray(current)) return null;
    if (current.model || current.resolution || current.task_type || current.usage || current.content) return current;
    if (current.task && typeof current.task === "object" && !Array.isArray(current.task)) return current.task;
    if (current.data && typeof current.data === "object" && !Array.isArray(current.data)) {
      current = current.data;
      continue;
    }
    return current;
  }
  return null;
}

function h3OriginTaskForRegeneration(ctx) {
  const origins = ctx && ctx.originTasks;
  if (!Array.isArray(origins) || origins.length !== 1) throw new Error(H3_MODEL + " regeneration source task is unavailable");
  const origin = origins[0];
  if (!origin || trimmed(origin.status).toUpperCase() !== "SUCCESS") throw new Error(H3_MODEL + " regeneration source task must be successful");
  const upstreamTaskId = trimmed(origin.upstreamTaskId);
  if (!upstreamTaskId) throw new Error(H3_MODEL + " regeneration source task has no upstream task ID");
  const payload = h3TaskPayload(origin.data);
  if (!payload || trimmed(payload.model) !== H3_MODEL) throw new Error(H3_MODEL + " regeneration requires a successful H3 source task");
  if (trimmed(payload.resolution).toUpperCase() !== "768P") throw new Error(H3_MODEL + " regeneration requires a 768P source task");
  if (trimmed(payload.task_type).toLowerCase() === H3_REGENERATION_OPERATION)
    throw new Error(H3_MODEL + " regeneration cannot use a regeneration task as its source");
  return { origin: origin, payload: payload, upstreamTaskId: upstreamTaskId };
}

function h3PayloadSeconds(payload) {
  const usage = payload && payload.usage && typeof payload.usage === "object" && !Array.isArray(payload.usage) ? payload.usage : {};
  const candidates = [payload && payload.duration, usage.output_seconds, usage.total_seconds];
  for (const candidate of candidates) {
    const seconds = Number(candidate);
    if (Number.isFinite(seconds) && seconds >= H3_MIN_DURATION && seconds <= H3_MAX_DURATION) return seconds;
  }
  return H3_MAX_DURATION;
}

function h3OptionalRegenerationFields(req, body) {
  const metadata = (req && req.metadata) || {};
  for (const key of ["callback_url", "aigc_watermark"]) {
    const value = req && req[key] !== undefined ? req[key] : metadata[key];
    if (value !== undefined && value !== null) body[key] = value;
  }
}

// 把网关公开任务 ID 映射为已校验的上游任务 ID，避免把用户可见 ID 直接发给 MiniMax。
function h3BuildRegenerationBody(ctx, req) {
  const body = { model: ctx.upstreamModel || ctx.model || H3_MODEL, resolution: H3_REGENERATION_RESOLUTION };
  const sourceTaskId = h3SourceTaskID(req);
  h3RegenerationResolution(req);
  if (sourceTaskId) {
    if (h3HasGenerationContent(req)) throw new Error(H3_MODEL + " source_task_id cannot be combined with generation content");
    const source = h3OriginTaskForRegeneration(ctx);
    if (trimmed(source.origin.taskId) !== sourceTaskId) throw new Error(H3_MODEL + " regeneration source task does not match the resolved origin task");
    body.source_task_id = source.upstreamTaskId;
  } else {
    body.content = h3RegenerationContent(req);
  }
  h3OptionalRegenerationFields(req, body);
  return body;
}

// 提交阶段为再生成预留安全额度；无法从 URL 得知源视频时长时按 15 秒预留。
function h3RegenerationUsage(ctx, req) {
  const sourceTaskId = h3SourceTaskID(req);
  let seconds = H3_MAX_DURATION;
  let content = [];
  let sourceImageCount;
  if (sourceTaskId) {
    const source = h3OriginTaskForRegeneration(ctx);
    seconds = h3PayloadSeconds(source.payload);
    // 再生成会重新收取原素材费用。优先读取源任务实际总张数，缺失时按允许的上限预留。
    sourceImageCount = H3_MAX_REFERENCE_IMAGES;
    const rawCount = source.payload.usage && source.payload.usage.input_image_count;
    if ((typeof rawCount === "number" || typeof rawCount === "string") && String(rawCount).trim() !== "") {
      const count = Number(rawCount);
      if (Number.isInteger(count) && count >= 0 && count <= H3_MAX_REFERENCE_IMAGES) sourceImageCount = count;
    }
  } else {
    content = h3RegenerationContent(req);
    seconds = h3RegenerationRequestedDuration(req);
  }
  const inputImages = content.filter(function (item) {
    return item && item.type === "image_url";
  }).length;
  const hasReferenceVideo = content.some(function (item) {
    return item && item.type === "video_url" && trimmed(item.role) !== "base_video";
  });
  return {
    seconds: seconds,
    resolution: H3_REGENERATION_RESOLUTION,
    input_images: sourceImageCount === undefined ? inputImages : sourceImageCount,
    input_video_seconds: hasReferenceVideo ? H3_MAX_INPUT_VIDEO_SECONDS : 0,
    operation: H3_REGENERATION_OPERATION,
    // 视频操作不产生 Context-IR Token；显式零值兼容编辑器保留的零价项。
    prompt_tokens: 0,
    completion_tokens: 0,
  };
}

// 上游未提供预估接口：输入按文本和媒体复杂度估算，输出独立预留，完成后逐项结算。
function h3ContextIRUsage(ctx, req) {
  if (ctx.usagePurpose === "billing_ratios") return null;
  const content = h3Content(req);
  let textCharacters = 0;
  let mediaCount = 0;
  for (const item of content) {
    if (item && item.type === "text") textCharacters += String(item.text || "").length;
    else if (item && (item.type === "image_url" || item.type === "video_url" || item.type === "audio_url")) mediaCount += 1;
  }
  // 输入与输出之和保持有界；新提交只提供已声明的两项用量，避免触发未声明数量的宿主上限。
  const promptTokens = Math.min(H3_CONTEXT_IR_MAX_TOKENS - H3_CONTEXT_IR_ESTIMATED_OUTPUT_TOKENS, Math.ceil(textCharacters / 2) + mediaCount * 1024);
  return {
    prompt_tokens: promptTokens,
    completion_tokens: H3_CONTEXT_IR_ESTIMATED_OUTPUT_TOKENS,
    operation: H3_CONTEXT_IR_OPERATION,
    // Context-IR 只生成文本，这些视频计费数量为零，不影响输入 Token 的媒体估算。
    seconds: 0,
    input_images: 0,
    input_video_seconds: 0,
  };
}

// 识别上游 task_type 和网关持久化 action，确保结算阶段仍保留再生成维度。
function h3CompletionOperation(task, body) {
  // 持久化动作是提交时的计费身份，上游回包不能把视频切换成另一种低价操作。
  const action = trimmed(task && task.action).toLowerCase();
  if (action === H3_REGENERATION_ACTION) return H3_REGENERATION_OPERATION;
  if (action === H3_CONTEXT_IR_ACTION) return H3_CONTEXT_IR_OPERATION;
  if (["text_to_video", "image_to_video", "first_tail_to_video", "reference_to_video"].includes(action)) return H3_GENERATION_OPERATION;
  const payload = h3QueryTask(body);
  const taskType = trimmed(payload && payload.task_type).toLowerCase();
  if (taskType === H3_REGENERATION_OPERATION) return H3_REGENERATION_OPERATION;
  if (taskType === H3_CONTEXT_IR_TASK_TYPE) return H3_CONTEXT_IR_OPERATION;
  return H3_GENERATION_OPERATION;
}

// 协议解码统一处理两种官方再生成模式，并将源任务交给宿主做权限和渠道校验。
function h3DecodeRegenerationRequest(req, model, fallbackPrompt, upstreamModel) {
  if (!isH3(model) && !isH3(upstreamModel)) return null;
  const hasSourceTaskId = req && Object.prototype.hasOwnProperty.call(req, "source_task_id");
  const sourceTaskId = h3SourceTaskID(req);
  const hasBaseVideo = h3HasBaseVideo(req);
  if (hasSourceTaskId) {
    if (!sourceTaskId) throw new Error(H3_MODEL + " source_task_id must be a non-empty string");
    if (h3HasGenerationContent(req)) {
      throw new Error(H3_MODEL + " source_task_id cannot be combined with generation content");
    }
  } else if (!hasBaseVideo) {
    return null;
  } else {
    h3RegenerationContent(req, fallbackPrompt);
  }
  h3RegenerationResolution(req);
  const requestBody = Object.assign({}, req, { model: model });
  // Responses input 提供的最终提示词必须保留到驱动阶段，不能只在解码校验时临时使用。
  if (!hasSourceTaskId) requestBody.content = h3RegenerationContent(req, fallbackPrompt);
  const intent = { kind: "submit", model: model, action: H3_REGENERATION_ACTION, requestBody: requestBody };
  if (hasSourceTaskId) intent.originTaskIds = [sourceTaskId];
  return intent;
}

// metadata.content 或顶层 content 都是完整多模态输入，统一走同一套校验。
function h3Content(req) {
  const metadata = req.metadata || {};
  const prompt = trimmed(req.prompt);
  const suppliedContent = h3RequestContent(req);
  if (suppliedContent !== undefined && suppliedContent !== null) {
    if (!Array.isArray(suppliedContent)) throw new Error("metadata.content must be an array");
    const items = suppliedContent;
    const hasText = items.some(function (item) {
      return item && item.type === "text" && trimmed(item.text);
    });
    if (hasText) return validateH3Content(items);
    if (!prompt) throw new Error(H3_MODEL + " metadata.content requires a text item or a prompt");
    return validateH3Content([{ type: "text", text: prompt }].concat(items));
  }
  const content = prompt ? [{ type: "text", text: prompt }] : [];
  for (const frame of h3FrameImages(req)) content.push(frame);
  const videos = h3MediaList(metadata, "reference_video");
  if (videos.length > H3_MAX_REFERENCE_VIDEOS) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_VIDEOS + " reference videos");
  for (const video of videos) content.push(h3MediaItem("video_url", video, "reference_video"));
  const audios = h3MediaList(metadata, "reference_audio");
  if (audios.length > H3_MAX_REFERENCE_AUDIOS) throw new Error(H3_MODEL + " accepts at most " + H3_MAX_REFERENCE_AUDIOS + " reference audios");
  for (const audio of audios) content.push(h3MediaItem("audio_url", audio, "reference_audio"));
  if (!content.length) throw new Error(H3_MODEL + " requires a prompt or a media input");
  return validateH3Content(content);
}

function h3HasVisualContent(content) {
  return content.some(function (item) {
    return item && (item.type === "image_url" || item.type === "video_url");
  });
}

// ratio is mandatory upstream and `adaptive` is only meaningful when the
// aspect ratio can be inherited from a visual input.
function h3Ratio(req, content) {
  const metadata = req.metadata || {};
  const ratio = trimmed(metadata.ratio) || trimmed(req.ratio);
  if (!ratio) return h3HasVisualContent(content) ? "adaptive" : "16:9";
  if (!H3_RATIOS.includes(ratio)) throw new Error(H3_MODEL + " ratio must be one of " + H3_RATIOS.join(", "));
  if (ratio === "adaptive" && !h3HasVisualContent(content)) throw new Error(H3_MODEL + " ratio adaptive requires an image or video input");
  return ratio;
}

// H3-Context-IR 与视频生成共用多模态 content 校验，但上游只返回增强提示词。
function h3ContextIRBody(req) {
  const content = h3Content(req);
  const metadata = req.metadata || {};
  const body = {
    model: H3_MODEL,
    content: content,
    duration: h3Duration(req),
    ratio: h3Ratio(req, content),
  };
  ["callback_url"].forEach(function (key) {
    const value = req[key] !== undefined ? req[key] : metadata[key];
    if (value !== undefined && value !== null) body[key] = value;
  });
  return body;
}

function h3QueryTask(body) {
  const task = body && typeof body === "object" && !Array.isArray(body) ? body.task : null;
  return task && typeof task === "object" && !Array.isArray(task) ? task : null;
}

function h3APIError(body) {
  const error = body && typeof body === "object" && !Array.isArray(body) ? body.error : null;
  if (!error || typeof error !== "object" || Array.isArray(error)) return null;
  const message = trimmed(error.message);
  if (!message) return null;
  const statusCode = Number(error.http_code || error.code || 0);
  return { message: message, statusCode: Number.isInteger(statusCode) ? statusCode : 0 };
}

// Older T2V-01*/I2V-01*/S2V-01 official tables disagree on 1080P support (research: 未验证).
// Keep those models permissive: duration 6 only, resolution optional.
function validateHailuoCombo(model, duration, resolution, hasImage) {
  if (isH3(model)) return;
  if (model === "MiniMax-Hailuo-2.3-Fast" && !hasImage) {
    throw new Error("MiniMax-Hailuo-2.3-Fast supports image-to-video only");
  }
  if (!isModernHailuo(model)) {
    if (duration !== undefined && Number(duration) !== 6) throw new Error(model + " duration must be 6");
    return;
  }
  const n = duration === undefined ? 6 : Number(duration);
  if (n !== 6 && n !== 10) throw new Error(model + " duration must be 6 or 10");
  if (n === 10) {
    if (model === "MiniMax-Hailuo-02" && hasImage) {
      if (resolution !== "768P" && resolution !== "512P") throw new Error("MiniMax-Hailuo-02 duration 10 only allows resolution 768P or 512P");
      return;
    }
    if (resolution !== "768P") throw new Error(model + " duration 10 only allows resolution 768P");
    return;
  }
  const allowed = model === "MiniMax-Hailuo-02" && hasImage ? ["512P", "768P", "1080P"] : ["768P", "1080P"];
  if (allowed.indexOf(resolution) < 0) throw new Error(model + " duration 6 only allows resolution " + allowed.join(" or "));
}

function responsesInput(req) {
  const texts = [],
    images = [];
  const input = req.input;
  if (typeof input === "string") texts.push(input);
  else if (Array.isArray(input)) {
    for (const item of input) {
      if (typeof item === "string") {
        texts.push(item);
        continue;
      }
      if (!item || typeof item !== "object" || Array.isArray(item)) continue;
      const content = item.content === undefined ? [item] : Array.isArray(item.content) ? item.content : [item.content];
      for (const part of content) {
        if (typeof part === "string") {
          texts.push(part);
          continue;
        }
        if (!part || typeof part !== "object" || Array.isArray(part)) continue;
        if (["input_text", "text"].includes(part.type) && typeof part.text === "string") texts.push(part.text);
        if (["input_image", "image_url"].includes(part.type)) {
          let image = part.image_url;
          if (image && typeof image === "object") image = image.url;
          if (trimmed(image)) images.push(trimmed(image));
        }
      }
    }
  }
  return {
    prompt: texts
      .filter(function (text) {
        return trimmed(text);
      })
      .join("\n"),
    images: images,
  };
}

function responsesVideoText(ctx) {
  const artifact = ctx && ctx.artifacts && ctx.artifacts.video;
  const url = trimmed(artifact && artifact.url);
  if (!url) throw new Error("video artifact is unavailable");
  const escaped = url.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return '<video controls src="' + escaped + '"></video>';
}

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody || {};
  const model = ctx.upstreamModel;
  const metadata = req.metadata || {};
  if (isH3(model)) {
    if (ctx.action === H3_CONTEXT_IR_ACTION) {
      return {
        url: ctx.baseUrl + "/v2/h3_context_ir",
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
        body: h3ContextIRBody(req),
        action: H3_CONTEXT_IR_ACTION,
      };
    }
    if (ctx.action === H3_REGENERATION_ACTION || h3RegenerationRequested(req)) {
      return {
        url: ctx.baseUrl + "/v2/video_regeneration",
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
        body: h3BuildRegenerationBody(ctx, req),
        action: H3_REGENERATION_ACTION,
      };
    }
    const content = h3Content(req);
    const h3Body = {
      model: model,
      content: content,
      resolution: h3Resolution(req),
      duration: h3Duration(req),
      ratio: h3Ratio(req, content),
    };
    ["callback_url", "aigc_watermark"].forEach(function (key) {
      // 原生接口使用顶层字段；兼容接口仍可通过 metadata 传递，显式 false 必须保留。
      const value = req[key] !== undefined ? req[key] : metadata[key];
      if (value !== undefined && value !== null) h3Body[key] = value;
    });
    return {
      url: ctx.baseUrl + "/v2/video_generation",
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
      body: h3Body,
      action: h3HasVisualContent(content) ? "image_to_video" : "text_to_video",
    };
  }
  const body = {
    model: model,
    prompt: req.prompt || undefined,
    duration: outboundDuration(req),
    resolution: outboundResolution(req, model),
  };
  ["prompt_optimizer", "fast_pretreatment", "callback_url", "aigc_watermark", "first_frame_image", "last_frame_image", "subject_reference"].forEach(
    function (key) {
      if (metadata[key] !== undefined && metadata[key] !== null) body[key] = metadata[key];
    }
  );
  return {
    url: ctx.baseUrl + "/v1/video_generation",
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
    body: body,
    action: hasHailuoImage(req, false) ? "image_to_video" : "text_to_video",
  };
}

export function parseSubmitResponse(ctx, resp) {
  const body = resp.body || {};
  const apiError = isH3(ctx.upstreamModel) ? h3APIError(body) : null;
  if (apiError) throw new Error(apiError.message);
  const base = body.base_resp;
  // /v1 always wraps the create response in a base_resp envelope; /v2 returns a
  // bare task_id and only adds base_resp when the call is rejected.
  if (base) {
    if (base.status_code !== 0) throw new Error(base.status_msg || "hailuo submit failed");
  } else if (!isH3(ctx.upstreamModel)) {
    throw new Error("hailuo submit failed");
  }
  if (!body.task_id) throw new Error("missing task_id");
  return { taskId: body.task_id, taskData: body };
}

export function extractUsage(ctx) {
  if (ctx.usagePurpose === "billing_ratios") return null;
  const req = ctx.requestBody || {};
  const model = ctx.upstreamModel || req.model;
  if (isH3(model)) {
    if (ctx.action === H3_CONTEXT_IR_ACTION) return h3ContextIRUsage(ctx, req);
    if (ctx.action === H3_REGENERATION_ACTION || h3RegenerationRequested(req)) return h3RegenerationUsage(ctx, req);
    const content = h3Content(req);
    return {
      seconds: h3Duration(req),
      resolution: h3Resolution(req),
      input_images: content.filter(function (item) {
        return item && item.type === "image_url";
      }).length,
      operation: H3_GENERATION_OPERATION,
      // 不适用的数值字段必须存在，避免完整价格表达式执行 nil * 0。
      prompt_tokens: 0,
      completion_tokens: 0,
      // Input URLs do not expose duration. Reserve the documented total limit;
      // polling replaces it with usage.input_seconds after success.
      input_video_seconds: content.some(function (item) {
        return item && item.type === "video_url";
      })
        ? H3_MAX_INPUT_VIDEO_SECONDS
        : 0,
    };
  }
  return { seconds: outboundDuration(req), resolution: outboundResolution(req, model), input_images: 0, input_video_seconds: 0 };
}

export function buildQueryRequest(ctx) {
  // Polling carries no relay info; the host fills these identities from the
  // persisted task properties.
  const path = isH3(ctx.upstreamModel || ctx.model)
    ? "/v2/query/video_generation/" + encodeURIComponent(ctx.taskId)
    : "/v1/query/video_generation?task_id=" + encodeURIComponent(ctx.taskId);
  return {
    url: ctx.baseUrl + path,
    method: "GET",
    headers: { Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
  };
}

export function parseTaskResult(ctx, body) {
  const apiError = h3APIError(body);
  if (apiError) {
    if (apiError.statusCode === 408 || apiError.statusCode === 429 || apiError.statusCode >= 500) throw new Error(apiError.message);
    return { code: apiError.statusCode, status: "FAILURE", progress: "100%", reason: apiError.message };
  }
  const h3Task = h3QueryTask(body);
  if (h3Task) {
    const h3Statuses = { queued: "QUEUED", running: "IN_PROGRESS", succeeded: "SUCCESS", failed: "FAILURE", cancelled: "FAILURE" };
    const h3Status = h3Statuses[h3Task.status];
    if (!h3Status) {
      return { status: "UNKNOWN", reason: "unrecognized status: " + String(h3Task.status || "") };
    }
    const h3Result = { code: 0, status: h3Status, progress: h3Status === "QUEUED" ? "30%" : h3Status === "IN_PROGRESS" ? "50%" : "100%" };
    if (h3Status === "SUCCESS") {
      const url = trimmed(h3Task.content && h3Task.content.url);
      if (url) h3Result.url = url;
    }
    if (h3Status === "FAILURE") {
      // 与主动取消入口使用相同标记，后台轮询确认的取消也可按 cancelled 查询。
      h3Result.reason =
        h3Task.status === "cancelled" ? "task cancelled by user" : trimmed(h3Task.error && h3Task.error.message) || "task " + trimmed(h3Task.status);
    }
    return h3Result;
  }
  if (body.base_resp && body.base_resp.status_code !== 0) {
    return { code: body.base_resp.status_code || 0, status: "FAILURE", progress: "100%", reason: body.base_resp.status_msg || "" };
  }
  const base = body.base_resp || {};
  const statuses = { Preparing: "IN_PROGRESS", Queueing: "IN_PROGRESS", Processing: "IN_PROGRESS", Success: "SUCCESS", Fail: "FAILURE" };
  const status = statuses[body.status];
  if (!status) {
    return { status: "UNKNOWN", reason: "unrecognized status: " + String(body.status || "") };
  }
  const progress = status === "SUCCESS" || status === "FAILURE" ? "100%" : body.status === "Processing" ? "50%" : "30%";
  const reason = status === "FAILURE" ? "task failed" : "";
  return { code: base.status_code || 0, status: status, progress: progress, reason: reason };
}

function artifactData(ctx) {
  const data = (ctx && ctx.data) || {};
  if (data.data && typeof data.data === "object" && data.data.task_id && Object.prototype.hasOwnProperty.call(data.data, "data")) return data.data.data || {};
  return data;
}

function artifactFileID(ctx) {
  return trimmed(artifactData(ctx).file_id);
}

// /v2 tasks expose a public CDN URL instead of a downloadable file id.
function h3ArtifactURL(ctx) {
  const task = h3QueryTask(artifactData(ctx));
  return task ? trimmed(task.content && task.content.url) : "";
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  return artifactFileID(task) || h3ArtifactURL(task) ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  const fileID = artifactFileID(ctx);
  if (!fileID) {
    const url = h3ArtifactURL(ctx);
    if (!url) throw new Error("artifact_not_found");
    return { url: url, method: ctx.clientRequest.method, credentialless: true };
  }
  return {
    url: ctx.baseUrl + "/v1/files/download?file_id=" + encodeURIComponent(fileID),
    method: ctx.clientRequest.method,
    headers: { Accept: "video/*", Authorization: "Bearer " + ctx.apiKey },
  };
}

export function extractUsageOnComplete(task, _taskResult, body) {
  const h3Task = h3QueryTask(body);
  if (h3Task) {
    const resolution = trimmed(h3Task.resolution).toUpperCase();
    const facts = { operation: h3CompletionOperation(task, body) };
    if (facts.operation === H3_CONTEXT_IR_OPERATION) {
      // 同时修复升级前缺少这些字段的冻结快照；缺失的实际 Token 仍保留估算。
      facts.seconds = 0;
      facts.input_images = 0;
      facts.input_video_seconds = 0;
      const usage = h3Task.usage && typeof h3Task.usage === "object" && !Array.isArray(h3Task.usage) ? h3Task.usage : {};
      // null、空字符串和布尔值不是实际用量，保留预扣估算，防止被 Number 转成零后全退。
      // 总量仅保留给升级前冻结的结算快照；新定价只使用输入、输出，不从总量推算缺失的一侧。
      const fields = { tokens: "total_tokens", prompt_tokens: "prompt_tokens", completion_tokens: "completion_tokens" };
      for (const key of Object.keys(fields)) {
        const rawTokens = usage[fields[key]];
        if ((typeof rawTokens !== "number" && typeof rawTokens !== "string") || String(rawTokens).trim() === "") continue;
        const tokens = Number(rawTokens);
        if (Number.isInteger(tokens) && tokens >= 0 && tokens <= H3_CONTEXT_IR_MAX_TOKENS) facts[key] = tokens;
      }
      return facts;
    }
    // 只补齐本操作不适用的字段，不能将缺失的实际视频用量填零后错误退款。
    facts.prompt_tokens = 0;
    facts.completion_tokens = 0;
    if (resolution === "2K" || resolution === "768P") facts.resolution = resolution;
    const usage = h3Task.usage && typeof h3Task.usage === "object" && !Array.isArray(h3Task.usage) ? h3Task.usage : {};
    const fields = [
      { key: "seconds", value: usage.output_seconds, minimum: H3_MIN_DURATION, maximum: H3_MAX_DURATION, integer: false },
      { key: "input_images", value: usage.input_image_count, minimum: 0, maximum: H3_MAX_REFERENCE_IMAGES, integer: true },
      { key: "input_video_seconds", value: usage.input_seconds, minimum: 0, maximum: H3_MAX_INPUT_VIDEO_SECONDS, integer: false },
    ];
    // Omit malformed or out-of-contract upstream values so settlement keeps
    // the bounded submission estimate instead of accepting a new multiplier.
    for (const field of fields) {
      // 只接受数值或非空数字字符串；Number(false)、Number([]) 不能覆盖预留用量。
      if ((typeof field.value !== "number" && typeof field.value !== "string") || String(field.value).trim() === "") continue;
      const value = Number(field.value);
      if (!Number.isFinite(value) || value < field.minimum || value > field.maximum || (field.integer && !Number.isInteger(value))) continue;
      facts[field.key] = value;
    }
    return Object.keys(facts).length ? facts : null;
  }
  const width = Number((body || {}).video_width || 0);
  const height = Number((body || {}).video_height || 0);
  if (!(width > 0) || !(height > 0)) return null;
  return { resolution: resolutionFor(width + "x" + height, "") };
}

function nativeJSONBody(ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
  const body = ctx.body.value;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
  return body;
}

function nativeH3Model(body) {
  const model = trimmed(body.model);
  if (model !== H3_MODEL) throw new Error("model must be " + H3_MODEL);
  return model;
}

function nativeH3VideoRequest(body) {
  const model = nativeH3Model(body);
  if (!Array.isArray(body.content)) throw new Error("content must be an array");
  const content = validateH3Content(body.content);
  if (!Object.prototype.hasOwnProperty.call(body, "duration")) throw new Error("duration is required");
  if (!Object.prototype.hasOwnProperty.call(body, "resolution")) throw new Error("resolution is required");
  const requestBody = {
    model: model,
    content: content,
    resolution: h3Resolution(body),
    duration: h3Duration(body),
    ratio: h3Ratio(body, content),
  };
  ["callback_url", "aigc_watermark"].forEach(function (key) {
    if (body[key] !== undefined && body[key] !== null) requestBody[key] = body[key];
  });
  return requestBody;
}

function nativeH3TaskStatus(task) {
  if (task && trimmed(task.fail_reason).toLowerCase() === "task cancelled by user") return "cancelled";
  const statuses = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "running", SUCCESS: "succeeded", FAILURE: "failed" };
  return statuses[task && task.status] || "queued";
}

function nativeH3Task(task) {
  const source = task && task.data && typeof task.data === "object" && !Array.isArray(task.data) ? task.data.task : null;
  const result = source && typeof source === "object" && !Array.isArray(source) ? Object.assign({}, source) : {};
  const model = trimmed(result.model) || H3_MODEL;
  const action = trimmed(task && task.action).toLowerCase();
  result.id = task && task.task_id ? task.task_id : trimmed(result.id);
  result.model = model;
  result.status = nativeH3TaskStatus(task);
  if (!result.created_at && task && task.created_at) result.created_at = task.created_at;
  if (!result.updated_at && task && task.updated_at) result.updated_at = task.updated_at;
  if (!result.task_type)
    result.task_type =
      action === H3_CONTEXT_IR_ACTION ? H3_CONTEXT_IR_TASK_TYPE : action === H3_REGENERATION_ACTION ? H3_REGENERATION_OPERATION : H3_GENERATION_OPERATION;
  if (!result.modality) result.modality = result.task_type === H3_CONTEXT_IR_TASK_TYPE ? "text" : "video";
  if (result.status === "failed" && !result.error) result.error = { message: trimmed(task && task.fail_reason) || "task failed" };
  return result;
}

// native 路由输出官方 H3 包络，同时只向调用方暴露网关公开任务 ID。
export const native = {
  createH3VideoTask: function (ctx) {
    const body = nativeJSONBody(ctx);
    const requestBody = nativeH3VideoRequest(body);
    return { kind: "submit", model: H3_MODEL, action: h3HasVisualContent(requestBody.content) ? "image_to_video" : "text_to_video", requestBody: requestBody };
  },
  createH3ContextIRTask: function (ctx) {
    const body = nativeJSONBody(ctx);
    nativeH3Model(body);
    if (!Array.isArray(body.content)) throw new Error("content must be an array");
    if (!Object.prototype.hasOwnProperty.call(body, "duration")) throw new Error("duration is required");
    const requestBody = h3ContextIRBody(body);
    return { kind: "submit", model: H3_MODEL, action: H3_CONTEXT_IR_ACTION, requestBody: requestBody };
  },
  createH3RegenerationTask: function (ctx) {
    const body = nativeJSONBody(ctx);
    if (!Object.prototype.hasOwnProperty.call(body, "resolution")) throw new Error("resolution is required");
    const intent = h3DecodeRegenerationRequest(body, nativeH3Model(body), "", H3_MODEL);
    if (!intent) throw new Error(H3_MODEL + " regeneration requires a source task or base_video");
    return intent;
  },
  decodeH3TaskList: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const query = ctx.query || {};
    const modelValues = query["filter.model"] || [];
    if (modelValues.length > 1) throw new Error("filter.model must be provided once");
    const model = trimmed(modelValues[0]) || H3_MODEL;
    if (model !== H3_MODEL) throw new Error("filter.model must be " + H3_MODEL);
    const taskIds = query["filter.task_ids"] || [];
    if (taskIds.length > 100) throw new Error("filter.task_ids accepts at most 100 task IDs");
    return {
      kind: "query",
      model: model,
      taskIds: taskIds.map(function (taskId) {
        return trimmed(taskId);
      }),
    };
  },
  decodeH3TaskDelete: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const taskId = trimmed(ctx.params && ctx.params.task_id);
    if (!taskId) throw new Error("task_id is required");
    return { kind: "delete", model: H3_MODEL, taskId: taskId };
  },
  h3TaskCreated: function (ctx, task) {
    return { task_id: task.task_id };
  },
  h3TaskStatus: function (ctx, task) {
    return { task: nativeH3Task(task) };
  },
  h3TaskList: function (ctx, result) {
    const items = result && Array.isArray(result.items) ? result.items.map(nativeH3Task) : [];
    return { items: items, total: Number(result && result.total) || 0 };
  },
  h3TaskDeleted: function (ctx, result) {
    return { task_id: result.task_id, action: result.action, status: result.status };
  },
  error: function (ctx, error) {
    return { type: "error", error: { type: error.code, message: error.message, http_code: String(error.httpStatus) } };
  },
};

export function buildTaskActionRequest(ctx) {
  if (ctx.operation !== "delete") throw new Error("unsupported task operation");
  if (!isH3(ctx.upstreamModel || ctx.model)) throw new Error("task operation requires " + H3_MODEL);
  return {
    url: ctx.baseUrl + "/v2/video_generation/" + encodeURIComponent(ctx.taskId),
    method: "DELETE",
    headers: { Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
  };
}

export const protocols = {
  openai_responses: {
    decodeRequest: function (ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
      const req = ctx.body.value;
      if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be an object");
      const model = trimmed(req.model);
      if (!model) throw new Error("model is required");
      if (req.input !== undefined && typeof req.input !== "string" && !Array.isArray(req.input)) throw new Error("input must be a string or array");
      if (req.images !== undefined && !Array.isArray(req.images)) throw new Error("images must be an array");
      if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata)))
        throw new Error("metadata must be an object");
      const input = responsesInput(req);
      const regeneration = h3DecodeRegenerationRequest(req, model, input.prompt, ctx.upstreamModel);
      if (regeneration) return regeneration;
      const prompt = input.prompt || trimmed(req.prompt);
      const images = [];
      for (const image of [req.image, req.input_reference].concat(req.images || [], input.images)) {
        if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
      }
      if (!prompt && images.length === 0) throw new Error("input is required");
      const metadata = Object.assign({}, req.metadata || {});
      if (images.length && !metadata.first_frame_image) metadata.first_frame_image = images[0];
      if (images.length > 1 && !metadata.last_frame_image) metadata.last_frame_image = images[1];
      const requestBody = { model: model, prompt: prompt, metadata: metadata };
      if (images.length) requestBody.images = images;
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.duration = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.duration = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      else if (Object.prototype.hasOwnProperty.call(req, "resolution")) requestBody.size = req.resolution;
      return { kind: "submit", model: model, action: images.length ? "image_to_video" : "text_to_video", requestBody: requestBody };
    },
    renderEvents: function (ctx, task, previousState) {
      const status = String(task.status || "UNKNOWN").toUpperCase();
      const value = Number(String(task.progress || "").replace("%", ""));
      const progress = Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
      const state = { status: status, progress: progress };
      if (status === "SUCCESS") {
        const text = responsesVideoText(ctx);
        const events = previousState && previousState.status === status ? [] : [{ type: "output", data: text }];
        return { events: events, state: state, done: true };
      }
      if (status === "FAILURE")
        return { events: [{ type: "error", code: "task_failed", message: task.fail_reason || "task failed" }], state: state, done: true };
      if (previousState && previousState.status === status && previousState.progress === progress) return { events: [], state: state, done: false };
      const event = { type: "progress", message: status.toLowerCase() };
      if (progress !== null) event.progress = progress;
      return { events: [event], state: state, done: false };
    },
    renderFinal: function (ctx, _task) {
      return {
        output: [
          {
            type: "message",
            status: "completed",
            role: "assistant",
            content: [{ type: "output_text", text: responsesVideoText(ctx), annotations: [], logprobs: [] }],
          },
        ],
        metadata: { vendor: "hailuo" },
      };
    },
  },
};

const legacyRenderers = {
  openai_video: function (task) {
    const statuses = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
    const output = {
      id: task.task_id,
      object: "video",
      model: task.properties && task.properties.origin_model_name ? task.properties.origin_model_name : "",
      status: statuses[task.status] || "unknown",
      progress: Number(String(task.progress || "0").replace("%", "")),
      created_at: task.created_at,
    };
    if (task.updated_at) output.completed_at = task.updated_at;
    if (task.data && task.data.base_resp && task.data.base_resp.status_code !== 0) {
      output.error = { message: task.data.base_resp.status_msg, code: String(task.data.base_resp.status_code) };
    }
    return output;
  },
};

protocols.openai_video = {
  decodeRequest: function (ctx) {
    if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
    let req;
    let hasInputReferenceFile = false;
    if (ctx.body.kind === "json") {
      if (!ctx.body.value || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
      req = Object.assign({}, ctx.body.value);
    } else {
      const first = function (name) {
        const values = (ctx.body.fields || {})[name] || [];
        if (values.length > 1) throw new Error(name + " must be provided once");
        return values[0];
      };
      req = {};
      const fields = ctx.body.fields || {};
      for (const name of Object.keys(fields)) {
        req[name] = first(name);
      }
      for (const file of ctx.body.files || []) {
        if (file.field !== "input_reference") throw new Error("unexpected file field: " + file.field);
        if (hasInputReferenceFile) throw new Error("input_reference must be provided once");
        hasInputReferenceFile = true;
      }
      if (req.metadata !== undefined) {
        let parsed;
        try {
          parsed = JSON.parse(req.metadata);
        } catch (e) {
          throw new Error("metadata must be a JSON object string");
        }
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("metadata must be a JSON object string");
        req.metadata = parsed;
      }
      if (req.seconds !== undefined) req.seconds = Number(req.seconds);
      else if (req.duration !== undefined) req.seconds = Number(req.duration);
    }
    if (hasInputReferenceFile && (Object.prototype.hasOwnProperty.call(req, "source_task_id") || h3HasBaseVideo(req))) {
      throw new Error(H3_MODEL + " regeneration currently requires a JSON content body");
    }
    const regeneration = h3DecodeRegenerationRequest(req, ctx.model, "", ctx.upstreamModel);
    if (regeneration) return regeneration;
    const seconds = req.seconds === undefined ? req.duration : req.seconds;
    if (seconds !== undefined) req.duration = Number(seconds);
    if (hasInputReferenceFile) {
      req.metadata = Object.assign({}, req.metadata || {}, {
        first_frame_image: { __fileRef: "request_file:input_reference", encoding: "dataUrl", maxBytes: 20971520 },
      });
    } else {
      const image = trimmed(req.input_reference || req.image);
      if (image) {
        req.metadata = Object.assign({}, req.metadata || {});
        if (!req.metadata.first_frame_image) req.metadata.first_frame_image = image;
      }
    }
    const hasImage = hasHailuoImage(req, hasInputReferenceFile);
    const duration = req.duration === undefined ? undefined : Number(req.duration);
    const comboModel = ctx.upstreamModel || ctx.model;
    validateHailuoCombo(comboModel, duration, outboundResolution(req, comboModel), hasImage);
    return {
      kind: "submit",
      model: ctx.model,
      action: hasImage ? "image_to_video" : "text_to_video",
      requestBody: Object.assign({}, req, { model: ctx.model }),
    };
  },
  render: function (ctx, task) {
    return legacyRenderers.openai_video(task);
  },
};
