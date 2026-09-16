// Fast、Mini 的官方输出范围独立于标准版，元数据和提交校验共用此声明。
const LOW_RESOLUTION_MODELS = ["doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615"];
const LOW_RESOLUTIONS = ["480p", "720p"];
// 官方能力按模型族集中声明，所有入口和计费阶段都使用同一份矩阵。
const SEEDANCE_20_MODELS = ["doubao-seedance-2-0-260128", "doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615"];
const DOUBAO_RATIOS = ["adaptive", "21:9", "16:9", "4:3", "1:1", "3:4", "9:16"];
const DOUBAO_COMPATIBILITY_FIELDS = [
  "content",
  "frames",
  "ratio",
  "service_tier",
  "draft",
  "generate_audio",
  "return_last_frame",
  "output_format",
  "seed",
  "camera_fixed",
  "watermark",
  "priority",
  "execution_expires_after",
  "tools",
  "safety_identifier",
  "omni_reference_task_type",
];

export const meta = {
  apiVersion: 1,
  key: "doubao",
  name: "Doubao Video",
  icon: "Doubao.Color",
  description: {
    en: "Volcengine Doubao Seedance video generation (text-to-video, image-to-video, and video-to-video)",
    zh: "火山引擎豆包 Seedance 视频生成（文生视频、图生视频、视频生视频）",
  },
  version: "1.1.4",
  author: { name: "QuantumNous" },
  channelTypes: [54, 45], // VolcEngine-type channels serve Ark video models with the same wire format
  models: [
    "doubao-seedance-1-0-pro-250528",
    "doubao-seedance-1-0-lite-t2v",
    "doubao-seedance-1-0-lite-i2v",
    "doubao-seedance-1-5-pro-251215",
    "doubao-seedance-2-0-260128",
    "doubao-seedance-2-0-fast-260128",
    "doubao-seedance-2-0-mini-260615",
    "doubao-seedance-2-5-260628",
  ],
  fetchMode: "per_task",
  usageSchema: {
    // Upstream billing tokens (estimated at submit, actual on completion).
    tokens: {
      type: "number",
      unit: "token",
      description: { en: "Billing token unit price", zh: "计费 Token 单价" },
    },
    // Output video resolution; Seedance token unit price varies by resolution tier.
    resolution: {
      enum: ["480p", "720p", "1080p", "4k"],
      enumLabels: {
        "480p": { en: "480p", zh: "480p" },
        "720p": { en: "720p", zh: "720p" },
        "1080p": { en: "1080p", zh: "1080p" },
        "4k": { en: "4k", zh: "4k" },
      },
      description: { en: "Output video resolution", zh: "输出视频分辨率" },
    },
    // Whether the request includes reference video input; Seedance prices video-to-video tokens at a lower unit rate.
    video_input: {
      enum: ["none", "video"],
      enumLabels: { none: { en: "No reference video", zh: "无参考视频" }, video: { en: "With reference video", zh: "有参考视频" } },
      description: { en: "Reference video input", zh: "参考视频输入" },
    },
  },
  // Official Ark formula tokens = (input + output seconds) × W × H × 24 / 1024,
  // 16:9 max-pixel sizes, cross-checked against Volcengine price examples.
  usageExamples: [
    { label: "480p · 5s", facts: { tokens: 48038, resolution: "480p", video_input: "none" } },
    { label: "720p · 5s", facts: { tokens: 108000, resolution: "720p", video_input: "none" } },
    { label: "1080p · 5s", facts: { tokens: 243000, resolution: "1080p", video_input: "none" } },
    { label: "4k · 5s", facts: { tokens: 972000, resolution: "4k", video_input: "none" } },
    { label: "720p · 10s", facts: { tokens: 216000, resolution: "720p", video_input: "none" } },
    { label: "720p · 5s (+4s 输入视频)", facts: { tokens: 194400, resolution: "720p", video_input: "video" } },
  ],
  routes: [
    { method: "POST", path: "/doubao/api/v3/contents/generations/tasks", type: "submit", decode: "createTask", render: "taskCreated" },
    { method: "GET", path: "/doubao/api/v3/contents/generations/tasks/:task_id", type: "query", render: "taskStatus" },
    { method: "GET", path: "/doubao/api/v3/contents/generations/tasks", type: "dynamic", action: "list", decode: "listTasks", render: "taskList" },
    {
      method: "DELETE",
      path: "/doubao/api/v3/contents/generations/tasks/:task_id",
      type: "dynamic",
      action: "delete",
      decode: "deleteTask",
      render: "taskDeleted",
    },
  ],
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};

// profile 完整替换默认 schema/examples，避免价格矩阵和计算器仍展开无效分辨率。
meta.usageProfiles = [
  {
    models: LOW_RESOLUTION_MODELS,
    schema: Object.assign({}, meta.usageSchema, {
      resolution: {
        enum: LOW_RESOLUTIONS,
        description: meta.usageSchema.resolution.description,
      },
    }),
    examples: meta.usageExamples.filter((example) => LOW_RESOLUTIONS.includes(example.facts.resolution)),
  },
];

function trimmed(value) {
  return String(value || "").trim();
}

function draftTaskIds(content) {
  const ids = [];
  if (!Array.isArray(content)) return ids;
  for (const item of content) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue;
    if (item.type !== "draft_task") continue;
    const draft = item.draft_task;
    if (!draft || typeof draft !== "object" || Array.isArray(draft)) continue;
    const id = trimmed(draft.id);
    if (id) ids.push(id);
  }
  return ids;
}

function rewriteDraftTaskContent(content, originTasks) {
  if (!Array.isArray(content)) return content;
  return content.map(function (item) {
    if (!item || typeof item !== "object" || Array.isArray(item) || item.type !== "draft_task") return item;
    const draft = item.draft_task;
    if (!draft || typeof draft !== "object" || Array.isArray(draft) || !trimmed(draft.id)) return item;
    const publicId = trimmed(draft.id);
    let upstream = "";
    if (Array.isArray(originTasks)) {
      for (const task of originTasks) {
        if (task && task.taskId === publicId) {
          upstream = trimmed(task.upstreamTaskId);
          break;
        }
      }
    }
    if (!upstream) throw new Error("origin task is unavailable");
    return Object.assign({}, item, { draft_task: Object.assign({}, draft, { id: upstream }) });
  });
}

function normalizeResolution(value) {
  const raw = trimmed(value).toLowerCase();
  if (["480p", "720p", "1080p", "4k"].includes(raw)) return raw;
  const parts = raw.replace("*", "x").split("x");
  if (parts.length !== 2) return "720p";
  const max = Math.max(Number(parts[0]), Number(parts[1]));
  if (max >= 3840) return "4k";
  if (max >= 1920) return "1080p";
  if (max >= 1280) return "720p";
  return "480p";
}

function modelKey(model) {
  return trimmed(model).toLowerCase();
}

// 仅约束文档明确列出的模型；渠道别名和未列出的旧模型交给最终上游校验。
function modelCapabilities(model) {
  const name = modelKey(model);
  if (SEEDANCE_20_MODELS.includes(name)) {
    return {
      minDuration: 4,
      maxDuration: 15,
      autoDuration: true,
      frames: false,
      serviceTier: false,
      framesInput: true,
      lastFrame: true,
      reference: true,
      adaptiveRatio: true,
      generateAudio: true,
      priority: true,
    };
  }
  return null;
}

// 兼容接口以顶层参数为准，未提供时才回退 metadata，避免校验与发送取值不一致。
function requestField(req, key) {
  const metadata = req && req.metadata && typeof req.metadata === "object" && !Array.isArray(req.metadata) ? req.metadata : {};
  if (req && Object.prototype.hasOwnProperty.call(req, key)) return { present: true, value: req[key] };
  if (Object.prototype.hasOwnProperty.call(metadata, key)) return { present: true, value: metadata[key] };
  return { present: false, value: undefined };
}

// 原生 JSON 保持数值类型约束；multipart 和兼容接口可使用数值字符串。
function integerFieldValue(value, allowNumericString) {
  if (typeof value === "number") return Number.isSafeInteger(value) ? value : null;
  if (allowNumericString && typeof value === "string" && value.trim() !== "") {
    const parsed = Number(value);
    return Number.isSafeInteger(parsed) ? parsed : null;
  }
  return null;
}

// 统一原生、兼容接口及最终渠道映射后的分辨率校验，缺省时按模型可用上限预估。
function requestResolution(model, req) {
  const resolutionField = requestField(req, "resolution");
  const sizeField = requestField(req, "size");
  const rawValue = resolutionField.present ? resolutionField.value : sizeField.present ? sizeField.value : "";
  // 错误类型不能经trimmed的真假值处理变成缺省分辨率。
  if (rawValue !== undefined && rawValue !== null && typeof rawValue !== "string") throw new Error("resolution or size must be a string");
  const raw = trimmed(rawValue).toLowerCase();
  const restricted = LOW_RESOLUTION_MODELS.includes(modelKey(model));
  if (!raw) return restricted ? "720p" : "1080p";
  const recognized = ["480p", "720p", "1080p", "4k"].includes(raw) || /^\d+[x*]\d+$/.test(raw);
  if (!recognized) {
    if (restricted) throw new Error(model + " only supports 480p and 720p resolution");
    throw new Error("resolution must be 480p, 720p, 1080p, or 4k");
  }
  const resolution = normalizeResolution(raw);
  if (restricted && !LOW_RESOLUTIONS.includes(resolution)) {
    throw new Error(model + " only supports 480p and 720p resolution");
  }
  if (resolution === "4k" && modelCapabilities(model) && modelKey(model) !== "doubao-seedance-2-0-260128")
    throw new Error(model + " does not support 4k resolution");
  return resolution;
}

// metadata 与兼容接口顶层参数都可能携带计费数量，提交、预扣和结算必须执行同一模型边界。
function validateGenerationParameters(req, model, strictTypes) {
  const capabilities = modelCapabilities(model);
  const secondsField = requestField(req, "seconds");
  const durationField = requestField(req, "duration");
  const durationPresent = secondsField.present || durationField.present;
  const durationValue = secondsField.present ? secondsField.value : durationField.value;
  const duration = durationPresent ? integerFieldValue(durationValue, !strictTypes) : null;
  if (durationPresent) {
    const minimum = capabilities ? capabilities.minDuration : 2;
    const maximum = capabilities ? capabilities.maxDuration : 30;
    const autoDuration = capabilities ? capabilities.autoDuration : true;
    if (duration === null || (duration === -1 ? !autoDuration : duration < minimum || duration > maximum)) {
      throw new Error(
        capabilities
          ? model + " duration must be -1 or an integer between " + minimum + " and " + maximum
          : "duration must be -1 or an integer between 2 and 30"
      );
    }
  }
  if (secondsField.present && durationField.present) {
    const seconds = integerFieldValue(secondsField.value, !strictTypes);
    const officialDuration = integerFieldValue(durationField.value, !strictTypes);
    if (seconds === null || officialDuration === null || seconds !== officialDuration) throw new Error("seconds and duration must not conflict");
  }

  const framesField = requestField(req, "frames");
  if (framesField.present) {
    const frames = integerFieldValue(framesField.value, !strictTypes);
    // 缺少最终模型身份时保留兼容层的通用边界；已知模型必须遵守官方能力矩阵。
    if (capabilities && !capabilities.frames) throw new Error(model + " does not support frames");
    if (frames === null || frames < 29 || frames > 289 || (frames - 25) % 4 !== 0) {
      throw new Error("frames must be an integer between 29 and 289 matching 25 + 4n");
    }
  }

  const serviceTierField = requestField(req, "service_tier");
  if (serviceTierField.present) {
    const serviceTier = typeof serviceTierField.value === "string" ? trimmed(serviceTierField.value) : "";
    if (!["default", "flex"].includes(serviceTier)) throw new Error("service_tier must be default or flex");
    if (capabilities && !capabilities.serviceTier) throw new Error(model + " does not support service_tier");
    const draftField = requestField(req, "draft");
    if (draftField.present && draftField.value === true && serviceTier === "flex") throw new Error("draft tasks do not support flex service_tier");
  }

  const draftField = requestField(req, "draft");
  if (draftField.present && typeof draftField.value !== "boolean") throw new Error("draft must be a boolean");
  if (draftField.present && draftField.value === true) {
    if (capabilities && !capabilities.draft) throw new Error(model + " does not support draft tasks");
    const resolution = requestResolution(model, req);
    if (resolution !== "480p") throw new Error("draft tasks require 480p resolution");
    const lastFrameField = requestField(req, "return_last_frame");
    if (lastFrameField.present && lastFrameField.value === true) throw new Error("draft tasks do not support return_last_frame");
  }
}

// 只检查结构，不主动下载媒体；避免在同步请求热路径引入网络探测。
function mediaURL(item, key) {
  const media = item && item[key];
  if (!media || typeof media !== "object" || Array.isArray(media)) return "";
  return typeof media.url === "string" ? trimmed(media.url) : "";
}

// 将原生 content、兼容接口 images 和旧式图片字段合并成实际发送的数组。
function requestContent(req, includePrompt) {
  const contentField = requestField(req, "content");
  if (contentField.present && !Array.isArray(contentField.value)) throw new Error("content must be an array");
  const imagesField = requestField(req, "images");
  if (imagesField.present && !Array.isArray(imagesField.value)) throw new Error("images must be an array");
  const content = [];
  for (const key of ["input_reference", "image"]) {
    const field = requestField(req, key);
    if (field.present && field.value !== undefined) content.push({ type: "image_url", image_url: { url: field.value } });
  }
  for (const url of imagesField.present ? imagesField.value : []) content.push({ type: "image_url", image_url: { url: url } });
  if (contentField.present) for (const item of contentField.value) content.push(item);
  const hasText = content.some((item) => item && item.type === "text" && typeof item.text === "string" && trimmed(item.text));
  const hasReference = content.some((item) => item && item.type !== "text");
  if (includePrompt && !hasText && (trimmed(req && req.prompt) || !hasReference)) content.push({ type: "text", text: req && req.prompt ? req.prompt : "" });
  return content;
}

// 按火山方舟官方 content 规则校验类型、角色、数量和互斥场景。
function validateDoubaoContent(model, content) {
  if (!Array.isArray(content)) throw new Error("content must be an array");
  const capabilities = modelCapabilities(model);
  let hasText = false;
  let imageCount = 0;
  let videoCount = 0;
  let audioCount = 0;
  let draftCount = 0;
  let firstFrames = 0;
  let lastFrames = 0;
  let unlabeledImages = 0;
  let referenceImages = 0;
  let referenceVideos = 0;
  let referenceAudios = 0;
  for (const item of content) {
    if (!item || typeof item !== "object" || Array.isArray(item)) throw new Error("content items must be objects");
    if (item.role !== undefined && item.role !== null && typeof item.role !== "string") throw new Error("role must be a string");
    const type = trimmed(item.type);
    const role = trimmed(item.role);
    if (type === "text") {
      if (typeof item.text !== "string" || !trimmed(item.text)) throw new Error("text items must be non-empty strings");
      hasText = true;
      continue;
    }
    if (type === "image_url") {
      if (!mediaURL(item, "image_url")) throw new Error("image_url must include a URL");
      imageCount += 1;
      if (!role) {
        unlabeledImages += 1;
        firstFrames += 1;
        if (capabilities && !capabilities.framesInput) throw new Error(model + " does not support image input");
      } else if (role === "first_frame") {
        firstFrames += 1;
        if (capabilities && !capabilities.framesInput) throw new Error(model + " does not support first_frame");
      } else if (role === "last_frame") {
        lastFrames += 1;
        if (capabilities && !capabilities.lastFrame) throw new Error(model + " does not support last_frame");
      } else if (role === "reference_image") {
        referenceImages += 1;
        if (capabilities && !capabilities.reference) throw new Error(model + " does not support reference_image");
      } else {
        throw new Error("image role is invalid");
      }
      continue;
    }
    if (type === "video_url") {
      if (!mediaURL(item, "video_url")) throw new Error("video_url must include a URL");
      if (role !== "reference_video") throw new Error("video role must be reference_video");
      videoCount += 1;
      referenceVideos += 1;
      if (capabilities && !capabilities.reference) throw new Error(model + " does not support reference_video");
      continue;
    }
    if (type === "audio_url") {
      if (!mediaURL(item, "audio_url")) throw new Error("audio_url must include a URL");
      if (role !== "reference_audio") throw new Error("audio role must be reference_audio");
      audioCount += 1;
      referenceAudios += 1;
      if (capabilities && !capabilities.reference) throw new Error(model + " does not support reference_audio");
      continue;
    }
    if (type === "draft_task") {
      const draft = item.draft_task;
      if (!draft || typeof draft !== "object" || Array.isArray(draft) || typeof draft.id !== "string" || !trimmed(draft.id))
        throw new Error("draft_task.id must be a non-empty string");
      draftCount += 1;
      if (capabilities && !capabilities.draft) throw new Error(model + " does not support draft_task");
      continue;
    }
    throw new Error("content type is invalid");
  }
  if (!hasText && imageCount === 0 && videoCount === 0 && audioCount === 0 && draftCount === 0) throw new Error("content must include text or media");
  if (unlabeledImages > 0 && imageCount !== 1) throw new Error("an image role is required when multiple images are provided");
  if (firstFrames > 1) throw new Error("content accepts at most one first_frame image");
  if (lastFrames > 1) throw new Error("content accepts at most one last_frame image");
  if (capabilities && lastFrames > 0 && firstFrames === 0) throw new Error("last_frame requires first_frame");
  const maxImages = (capabilities && capabilities.referenceImages) || 9;
  const maxVideos = (capabilities && capabilities.referenceVideos) || 3;
  const maxAudios = (capabilities && capabilities.referenceAudios) || 3;
  if (capabilities && referenceImages > maxImages) throw new Error("content accepts at most " + maxImages + " reference images");
  if (capabilities && referenceVideos > maxVideos) throw new Error("content accepts at most " + maxVideos + " reference videos");
  if (capabilities && referenceAudios > maxAudios) throw new Error("content accepts at most " + maxAudios + " reference audios");
  if (firstFrames + lastFrames > 0 && referenceImages + referenceVideos + referenceAudios > 0)
    throw new Error("frame images cannot be mixed with reference media");
  if (capabilities && !capabilities.audioOnly && audioCount > 0 && imageCount + videoCount === 0)
    throw new Error("audio input requires an image or video input");
  if (draftCount > 0 && (draftCount !== 1 || content.length !== 1)) throw new Error("draft_task cannot be combined with other content");
  return {
    hasText: hasText,
    hasMedia: imageCount + videoCount + audioCount + draftCount > 0,
    hasVisual: imageCount + videoCount > 0,
    hasVideo: videoCount > 0,
    hasFrames: firstFrames + lastFrames > 0,
    hasReference: imageCount + videoCount + audioCount + draftCount > 0,
  };
}

// 校验官方顶层选项，防止兼容接口把明显无效的类型和范围交给上游。
function validateDoubaoOptions(model, req, contentInfo, strictTypes) {
  const capabilities = modelCapabilities(model);
  // 2.0真实请求明确拒绝mov；其他模型仍由各自上游能力决定。
  const outputFormat = requestField(req, "output_format");
  if (capabilities && outputFormat.present && outputFormat.value !== "mp4") throw new Error(model + " only supports mp4 output_format");
  // 回调只校验基本结构，不在同步请求中做网络探测；用户标识遵守官方64字符英文串上限。
  const callback = requestField(req, "callback_url");
  if (callback.present && (typeof callback.value !== "string" || !/^https?:\/\/[^\s/?#]+(?:[/?#][^\s]*)?$/.test(callback.value)))
    throw new Error("callback_url must be an HTTP or HTTPS URL");
  const safetyIdentifier = requestField(req, "safety_identifier");
  if (
    safetyIdentifier.present &&
    (typeof safetyIdentifier.value !== "string" || safetyIdentifier.value.length > 64 || /[^\x20-\x7e]/.test(safetyIdentifier.value))
  )
    throw new Error("safety_identifier must be an ASCII string of at most 64 characters");
  const ratioField = requestField(req, "ratio");
  if (ratioField.present && ratioField.value !== undefined && ratioField.value !== null && typeof ratioField.value !== "string")
    throw new Error("ratio must be a string");
  if (ratioField.present && ratioField.value !== undefined && ratioField.value !== null && trimmed(ratioField.value)) {
    const ratio = typeof ratioField.value === "string" ? trimmed(ratioField.value) : "";
    if (!DOUBAO_RATIOS.includes(ratio)) throw new Error("ratio must be adaptive, 21:9, 16:9, 4:3, 1:1, 3:4, or 9:16");
    if (ratio === "adaptive" && capabilities && !capabilities.adaptiveRatio && !(contentInfo && contentInfo.hasVisual))
      throw new Error(model + " adaptive ratio requires image input");
  }

  for (const key of ["generate_audio", "return_last_frame", "watermark", "camera_fixed"]) {
    const field = requestField(req, key);
    if (field.present && typeof field.value !== "boolean") throw new Error(key + " must be a boolean");
    // 官方文档与真实上游均确认 2.0 不接受该字段，提前拒绝可避免预扣和无效上游请求。
    if (key === "camera_fixed" && field.present && capabilities) throw new Error(model + " does not support camera_fixed");
    if (key === "generate_audio" && field.present && capabilities && !capabilities.generateAudio) throw new Error(model + " does not support generate_audio");
  }

  const seedField = requestField(req, "seed");
  if (seedField.present) {
    const seed = integerFieldValue(seedField.value, !strictTypes);
    if (seed === null || seed < -1 || seed > 4294967295) throw new Error("seed must be an integer between -1 and 4294967295");
  }
  const priorityField = requestField(req, "priority");
  if (priorityField.present) {
    const priority = integerFieldValue(priorityField.value, !strictTypes);
    if (priority === null || priority < 0 || priority > 9) throw new Error("priority must be an integer between 0 and 9");
    if (capabilities && !capabilities.priority) throw new Error(model + " does not support priority");
  }
  const expiryField = requestField(req, "execution_expires_after");
  if (expiryField.present) {
    const expiry = integerFieldValue(expiryField.value, !strictTypes);
    if (expiry === null || expiry < 3600 || expiry > 259200) throw new Error("execution_expires_after must be an integer between 3600 and 259200");
  }
  const toolsField = requestField(req, "tools");
  if (toolsField.present) {
    if (
      !Array.isArray(toolsField.value) ||
      toolsField.value.some((tool) => !tool || typeof tool !== "object" || Array.isArray(tool) || tool.type !== "web_search")
    )
      throw new Error("tools must be an array of web_search tools");
  }
}

function hasVideo(content) {
  return Array.isArray(content) && content.some((item) => item && (item.type === "video_url" || Object.prototype.hasOwnProperty.call(item, "video_url")));
}

// Max-pixel 16:9 dimensions per resolution tier. Used when ratio is absent or
// adaptive so the submit-time estimate overestimates rather than underestimates.
// Official Ark formula: tokens = seconds × width × height × 24 / 1024.
// Video input duration is omitted; extractUsageOnComplete overlays the real bill.
function resolutionMaxPixels(resolution) {
  if (resolution === "480p") return [854, 480];
  if (resolution === "1080p") return [1920, 1080];
  if (resolution === "4k") return [3840, 2160];
  return [1280, 720];
}

function estimateTokens(seconds, resolution) {
  const dims = resolutionMaxPixels(resolution);
  return (seconds * dims[0] * dims[1] * 24) / 1024;
}

// 明确的零用量与缺失不同；错误类型返回 null，禁止 JS 隐式转换造成低额结算。
function billingTokenCount(value) {
  if ((typeof value !== "number" && typeof value !== "string") || String(value).trim() === "") return null;
  const tokens = Number(value);
  return Number.isSafeInteger(tokens) && tokens >= 0 ? tokens : null;
}

function videoInputRatio(model, resolution, content) {
  const video = hasVideo(content);
  const res = trimmed(resolution).toLowerCase();
  if (model === "doubao-seedance-2-5-260628") {
    if (res === "1080p") return video ? 7.0 / 10.7 : 11.7 / 10.7;
    return video ? 42 / 70 : 1;
  }
  if (model === "doubao-seedance-2-0-260128") {
    if (res === "1080p") return video ? 31 / 46 : 51 / 46;
    if (res === "4k") return video ? 16 / 46 : 26 / 46;
    return video ? 28 / 46 : 1;
  }
  if (model === "doubao-seedance-2-0-fast-260128") return video ? 22 / 37 : 1;
  if (model === "doubao-seedance-2-0-mini-260615") return video ? 14 / 23 : 1;
  return 1;
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

// 原生单查和列表共用任务视图；取消后的本地状态优先于尚未更新的上游快照。
function nativeTask(task) {
  const data = task.data && typeof task.data === "object" && !Array.isArray(task.data) ? task.data : {};
  const output = Object.assign({}, data, { id: task.task_id });
  const statusMap = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "running", SUCCESS: "succeeded", FAILURE: "failed" };
  output.status = statusMap[task.status] || "queued";
  if (task.fail_reason === "task cancelled by user") output.status = "cancelled";
  else if (task.status === "FAILURE" && data.status === "expired") output.status = "expired";
  if (!output.created_at && task.created_at) output.created_at = task.created_at;
  if (!output.updated_at && task.updated_at) output.updated_at = task.updated_at;
  if (task.fail_reason && output.status === "failed" && !output.error) output.error = { message: task.fail_reason };
  return output;
}

export const native = {
  createTask: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
    const body = ctx.body.value;
    if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
    const model = trimmed(body.model);
    if (!model) throw new Error("model is required");
    requestResolution(model, body);
    validateGenerationParameters(body, model, true);
    if (!Array.isArray(body.content)) throw new Error("content must be an array");
    const content = body.content;
    const contentInfo = validateDoubaoContent(model, content);
    validateDoubaoOptions(model, body, contentInfo, true);
    const texts = [];
    for (const item of content) {
      if (item.type === "text" && typeof item.text === "string") texts.push(item.text);
    }
    const requestBody = {
      model: model,
      prompt: texts
        .filter(function (text) {
          return trimmed(text);
        })
        .join("\n"),
      metadata: body,
    };
    const seconds = Number(body.duration);
    if (Number.isFinite(seconds) && seconds > 0) requestBody.seconds = seconds;
    const intent = { kind: "submit", model: model, action: contentInfo.hasMedia ? "image_to_video" : "text_to_video", requestBody: requestBody };
    const originTaskIds = draftTaskIds(content);
    if (originTaskIds.length) intent.originTaskIds = originTaskIds;
    return intent;
  },
  taskCreated: function (ctx, task) {
    return { id: task.task_id };
  },
  taskStatus: function (ctx, task) {
    return nativeTask(task);
  },
  // 官方列表只覆盖最近七天；宿主按用户归属查询本地任务，避免共享渠道泄漏其他用户记录。
  listTasks: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const query = ctx.query || {};
    const allowed = ["page_num", "page_size", "filter.model", "filter.status", "filter.service_tier", "filter.task_ids"];
    for (const key of Object.keys(query)) {
      if (!allowed.includes(key)) throw new Error("unsupported query parameter: " + key);
      if (key !== "filter.task_ids" && query[key].length !== 1) throw new Error(key + " must be provided once");
    }
    const model = trimmed((query["filter.model"] || [""])[0]);
    if (model.length > 191) throw new Error("filter.model is too long");
    const serviceTier = (query["filter.service_tier"] || ["default"])[0];
    if (!["default", "flex"].includes(serviceTier)) throw new Error("filter.service_tier must be default or flex");
    const status = (query["filter.status"] || [""])[0];
    if (!["", "queued", "running", "cancelled", "succeeded", "failed"].includes(status)) throw new Error("filter.status is invalid");
    const pagination = {};
    for (const field of ["page_num", "page_size"]) {
      const raw = (query[field] || [field === "page_num" ? "1" : "20"])[0];
      if (!/^\d+$/.test(raw) || Number(raw) < 1 || Number(raw) > 500) throw new Error(field + " must be an integer between 1 and 500");
      pagination[field] = Number(raw);
    }
    const taskIds = query["filter.task_ids"] || [];
    if (taskIds.length > 100 || taskIds.some((id) => !trimmed(id) || id.length > 191)) throw new Error("filter.task_ids is invalid");
    return {
      kind: "query",
      model: model,
      taskIds: taskIds.map(trimmed),
      listOptions: { pageNum: pagination.page_num, pageSize: pagination.page_size, serviceTier: serviceTier, lookbackSeconds: 604800 },
    };
  },
  taskList: function (ctx, result) {
    return { items: result.items.map(nativeTask), total: result.total };
  },
  deleteTask: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const taskId = trimmed(ctx.params && ctx.params.task_id);
    if (!taskId) throw new Error("task_id is required");
    return { kind: "delete", taskId: taskId };
  },
  taskDeleted: function () {
    // 保持网关既有 JSON 响应；上游允许空响应体，由动作解析器处理。
    return {};
  },
  error: function (ctx, error) {
    return { error: { code: error.code, message: error.message } };
  },
};

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody;
  const model = ctx.upstreamModel || req.model || "";
  requestResolution(model, req);
  validateGenerationParameters(req, model, false);
  const metadata = req.metadata || {};
  const body = Object.assign({ model: req.model || "", content: [] }, metadata);
  // Responses/OpenAI 兼容层把官方选项放在顶层，这些字段必须进入最终上游请求。
  for (const key of DOUBAO_COMPATIBILITY_FIELDS) {
    if (Object.prototype.hasOwnProperty.call(req, key)) body[key] = req[key];
  }
  // 原生 content 的顺序、兼容接口图片和素材引用都必须原样保留。
  body.content = requestContent(req, true);
  if (Array.isArray(body.content)) body.content = rewriteDraftTaskContent(body.content, ctx.originTasks);
  // duration 别名和智能时长 -1 都必须真正发送；不能截断小数后丢失请求数量。
  const seconds = req.seconds !== undefined ? req.seconds : req.duration;
  if (seconds !== undefined) body.duration = Number(seconds);
  body.model = model;
  // 渠道别名映射后再次校验；兼容接口的 size/resolution 必须与实际发往上游的值一致。
  const resolutionField = requestField(req, "resolution");
  const sizeField = requestField(req, "size");
  if ((resolutionField.present && trimmed(resolutionField.value)) || (sizeField.present && trimmed(sizeField.value)))
    body.resolution = requestResolution(model, req);
  const contentInfo = validateDoubaoContent(model, body.content);
  validateDoubaoOptions(model, body, contentInfo, false);
  const hasReference = contentInfo.hasMedia;
  return {
    url: ctx.baseUrl + "/api/v3/contents/generations/tasks",
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
    body: body,
    action: hasReference ? "image_to_video" : "text_to_video",
    rewriteModel: body.model,
  };
}

export function parseSubmitResponse(ctx, resp) {
  if (!resp.body || !resp.body.id) throw new Error("task_id is empty");
  // 提交响应只有 ID，先保存服务等级，使第一次轮询前的列表筛选也准确。
  const serviceTier = requestField(ctx.requestBody, "service_tier");
  return { taskId: resp.body.id, taskData: Object.assign({ service_tier: serviceTier.value || "default" }, resp.body) };
}

export function extractUsage(ctx) {
  const req = ctx.requestBody || {};
  const model = ctx.upstreamModel || ctx.model || req.model || "";
  validateGenerationParameters(req, model, false);
  const content = requestContent(req, false);
  const contentInfo = content.length ? validateDoubaoContent(model, content) : { hasVisual: false };
  validateDoubaoOptions(model, req, contentInfo, false);
  const resolution = requestResolution(model, req);
  if (ctx.usagePurpose === "billing_ratios") {
    // 预估 token 可以保守取上限，但不得改变未指定分辨率时旧倍率计费的单价。
    const requestedResolution = requestField(req, "resolution");
    const requestedSize = requestField(req, "size");
    const ratio = videoInputRatio(model, requestedResolution.value || requestedSize.value ? resolution : "", content);
    return ratio === 1 ? null : { video_input_ratio: ratio };
  }
  // 官方 frames 优先于 duration；非整数秒不能向下取整后少预扣。
  const framesField = requestField(req, "frames");
  const frames = framesField.present ? integerFieldValue(framesField.value, true) : null;
  const secondsField = requestField(req, "seconds");
  const durationField = requestField(req, "duration");
  const durationValue = secondsField.present ? secondsField.value : durationField.value;
  let seconds = frames !== null && frames > 0 ? frames / 24 : Number(durationValue || 0);
  if (!Number.isFinite(seconds) || seconds <= 0) {
    seconds = modelKey(model) === "doubao-seedance-2-5-260628" ? 30 : 15;
  }
  if (seconds <= 0) seconds = 5;
  seconds = Math.min(seconds, 3600);
  return {
    tokens: estimateTokens(seconds, resolution),
    resolution: resolution,
    video_input: hasVideo(content) ? "video" : "none",
  };
}

export function buildQueryRequest(ctx) {
  return {
    url: ctx.baseUrl + "/api/v3/contents/generations/tasks/" + encodeURIComponent(ctx.taskId),
    method: "GET",
    headers: { Accept: "application/json", "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey },
  };
}

// 管理操作继续使用渠道密钥和真实上游任务 ID，客户端只持有网关公开 ID。
export function buildTaskActionRequest(ctx) {
  if (ctx.operation !== "delete") throw new Error("unsupported task operation");
  return {
    url: ctx.baseUrl + "/api/v3/contents/generations/tasks/" + encodeURIComponent(ctx.taskId),
    method: "DELETE",
    headers: { Accept: "application/json", "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey },
  };
}

// 空成功响应不区分取消和删除；非终态必须由宿主再次查询确认，不能凭提交前状态直接退款。
export function parseTaskActionResponse(ctx, response) {
  if (
    response.statusCode !== 200 ||
    (response.body !== null &&
      response.body !== undefined &&
      (typeof response.body !== "object" || Array.isArray(response.body) || Object.keys(response.body).length !== 0))
  ) {
    throw new Error("unexpected task deletion response");
  }
  return { action: ctx.status === "SUCCESS" || ctx.status === "FAILURE" ? "deleted" : "unknown" };
}

export function parseTaskResult(ctx, body) {
  if (body.status === "pending" || body.status === "queued") return { status: "QUEUED", progress: "10%" };
  if (body.status === "processing" || body.status === "running") return { status: "IN_PROGRESS", progress: "50%" };
  if (body.status === "succeeded") {
    const result = { status: "SUCCESS", progress: "100%", url: body.content && body.content.video_url ? body.content.video_url : "" };
    const usage = body.usage || {};
    const completionTokens = billingTokenCount(usage.completion_tokens);
    const totalTokens = billingTokenCount(usage.total_tokens);
    if (Number.isFinite(completionTokens) && completionTokens > 0) result.completionTokens = completionTokens;
    if (Number.isFinite(totalTokens) && totalTokens > 0) result.totalTokens = totalTokens;
    return result;
  }
  if (body.status === "failed" || body.status === "expired" || body.status === "cancelled") {
    const reason = body.status === "cancelled" ? "task cancelled by user" : body.error && body.error.message ? body.error.message : body.status;
    return { status: "FAILURE", progress: "100%", reason: reason };
  }
  return { status: "UNKNOWN", reason: "unrecognized status: " + String(body.status || "") };
}

function artifactData(ctx) {
  const data = (ctx && ctx.data) || {};
  if (data.data && typeof data.data === "object" && data.data.task_id && Object.prototype.hasOwnProperty.call(data.data, "data")) return data.data.data || {};
  return data;
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  const content = artifactData(task).content || {};
  const artifacts = [];
  if (trimmed(content.video_url)) artifacts.push({ key: "video", type: "video" });
  if (trimmed(content.last_frame_url)) artifacts.push({ key: "last_frame", type: "image", mimeType: "image/png" });
  return artifacts;
}

export function buildContentRequest(ctx) {
  const content = artifactData(ctx).content || {};
  const urls = { video: content.video_url, last_frame: content.last_frame_url };
  const url = trimmed(urls[ctx.artifactKey]);
  if (!url) throw new Error("artifact_not_found");
  return { url: url, method: ctx.clientRequest.method, credentialless: true };
}

export function extractUsageOnComplete(task, taskResult, body) {
  if (!body || body.status !== "succeeded") return {};
  const facts = {};
  const usage = body.usage || {};
  let tokens = billingTokenCount(usage.completion_tokens);
  const totalTokens = billingTokenCount(usage.total_tokens);
  // 明确的 completion_tokens=0 是有效计费用量，只有缺失或非法值才回退总量。
  if (tokens === null) tokens = totalTokens;
  if (tokens !== null) facts.tokens = tokens;
  const content = body.content || {};
  const resolution = trimmed(content.resolution || body.resolution).toLowerCase();
  if (["480p", "720p", "1080p", "4k"].includes(resolution)) facts.resolution = resolution;
  return facts;
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
      const prompt = input.prompt || trimmed(req.prompt);
      const images = [];
      for (const image of [req.image, req.input_reference].concat(req.images || [], input.images)) {
        if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
      }
      const metadata = Object.assign({}, req.metadata || {});
      if (Object.prototype.hasOwnProperty.call(req, "resolution")) metadata.resolution = req.resolution;
      else if (req.size && !metadata.resolution) metadata.resolution = normalizeResolution(req.size);
      const requestBody = { model: model, prompt: prompt, metadata: metadata };
      for (const key of DOUBAO_COMPATIBILITY_FIELDS) {
        if (Object.prototype.hasOwnProperty.call(req, key)) requestBody[key] = req[key];
      }
      if (images.length) requestBody.images = images;
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.seconds = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.seconds = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      const effectiveModel = ctx.upstreamModel || model;
      requestResolution(effectiveModel, requestBody);
      validateGenerationParameters(requestBody, effectiveModel, false);
      const content = requestContent(requestBody, true);
      const contentInfo = validateDoubaoContent(effectiveModel, content);
      validateDoubaoOptions(effectiveModel, requestBody, contentInfo, false);
      const intent = { kind: "submit", model: model, action: contentInfo.hasMedia ? "image_to_video" : "text_to_video", requestBody: requestBody };
      const originTaskIds = draftTaskIds(content);
      if (originTaskIds.length) intent.originTaskIds = originTaskIds;
      return intent;
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
        metadata: { vendor: "doubao" },
      };
    },
  },
};

const legacyRenderers = {
  openai_video: function (task) {
    const data = task.data || {};
    const statusMap = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
    const output = {
      id: task.task_id,
      object: "video",
      model: task.properties ? task.properties.origin_model_name || "" : "",
      status: statusMap[task.status] || "unknown",
      progress: Number(String(task.progress || "0").replace("%", "")),
      created_at: task.created_at,
      completed_at: task.updated_at,
    };
    if (data.status === "failed") output.error = { message: data.error ? data.error.message || "" : "", code: data.error ? data.error.code || "" : "" };
    return output;
  },
};

protocols.openai_video = {
  decodeRequest: function (ctx) {
    if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
    if (ctx.body.kind === "json") {
      if (!ctx.body.value || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
      const req = Object.assign({}, ctx.body.value);
      const model = ctx.upstreamModel || ctx.model;
      requestResolution(model, req);
      validateGenerationParameters(req, model, false);
      const content = requestContent(req, true);
      const contentInfo = validateDoubaoContent(model, content);
      validateDoubaoOptions(model, req, contentInfo, false);
      return {
        kind: "submit",
        model: ctx.model,
        action: contentInfo.hasMedia ? "image_to_video" : "text_to_video",
        requestBody: Object.assign({}, req, { model: ctx.model }),
      };
    }
    const first = function (name) {
      const values = (ctx.body.fields || {})[name] || [];
      if (values.length > 1) throw new Error(name + " must be provided once");
      return values[0];
    };
    const req = {};
    const fields = ctx.body.fields || {};
    for (const name of Object.keys(fields)) {
      req[name] = first(name);
    }
    // multipart 标量以字符串传输；恢复官方 JSON 类型后再校验和发送，保留 false/0。
    for (const key of ["generate_audio", "return_last_frame", "watermark", "camera_fixed", "draft"]) {
      if (req[key] === "true") req[key] = true;
      else if (req[key] === "false") req[key] = false;
    }
    for (const key of ["duration", "seconds", "frames", "seed", "priority", "execution_expires_after"]) {
      if (req[key] !== undefined) {
        const value = integerFieldValue(req[key], true);
        if (value === null) throw new Error(key + " must be an integer");
        req[key] = value;
      }
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
    if (typeof req.content === "string") {
      let parsed;
      try {
        parsed = JSON.parse(req.content);
      } catch (e) {
        throw new Error("content must be a JSON array string", { cause: e });
      }
      if (!Array.isArray(parsed)) throw new Error("content must be a JSON array string");
      req.content = parsed;
    }
    if ((ctx.body.files || []).length) throw new Error("Doubao requires image and video references to be URLs inside metadata.content");
    const model = ctx.upstreamModel || ctx.model;
    requestResolution(model, req);
    validateGenerationParameters(req, model, false);
    if (req.seconds !== undefined) req.seconds = Number(req.seconds);
    else if (req.duration !== undefined) req.seconds = Number(req.duration);
    const content = requestContent(req, true);
    const contentInfo = validateDoubaoContent(model, content);
    validateDoubaoOptions(model, req, contentInfo, false);
    return {
      kind: "submit",
      model: ctx.model,
      action: contentInfo.hasMedia ? "image_to_video" : "text_to_video",
      requestBody: Object.assign({}, req, { model: ctx.model }),
    };
  },
  render: function (ctx, task) {
    return legacyRenderers.openai_video(task);
  },
};
