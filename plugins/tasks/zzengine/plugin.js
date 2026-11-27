const SEEDANCE_20_MODELS = [
  "doubao-seedance-2-0-260128",
  "doubao-seedance-2-0-260128-ob",
  "doubao-seedance-2-0-fast-260128",
  "doubao-seedance-2-0-mini-260615",
];
const SEEDANCE_25_FLASH_MODELS = ["doubao-seedance-2-5-flash-261115"];
const SEEDANCE_25_MODELS = ["doubao-seedance-2-5-260628", "doubao-seedance-2-5-260628-ob", ...SEEDANCE_25_FLASH_MODELS];
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
  "callback_url",
  "moderation_options",
];
const VIDEO_MODELS = {
  "doubao-seedance-2-0-260128": { resolutions: ["480p", "720p", "1080p", "4k"], videoInput: true },
  "doubao-seedance-2-0-260128-ob": { resolutions: ["480p", "720p", "1080p", "4k"], videoInput: true },
  "doubao-seedance-2-0-fast-260128": { resolutions: ["480p", "720p"], videoInput: true },
  "doubao-seedance-2-0-mini-260615": { resolutions: ["480p", "720p"], videoInput: true },
  "doubao-seedance-2-5-260628": { resolutions: ["480p", "720p", "1080p", "4k"], videoInput: true },
  "doubao-seedance-2-5-260628-ob": { resolutions: ["480p", "720p", "1080p", "4k"], videoInput: true },
  "doubao-seedance-2-5-flash-261115": { resolutions: ["480p", "720p"], videoInput: true },
};
const SEEDANCE_RESOLUTIONS = ["480p", "720p", "1080p", "4k"];
const DEFAULT_VIDEO_PROFILE = { resolutions: SEEDANCE_RESOLUTIONS, videoInput: true };
const VIDEO_FPS = 24;
// 上游整数时长会向下取整，预扣覆盖不足一秒的额外帧。
const VIDEO_RESERVE_EXTRA_FRAMES = VIDEO_FPS - 1;

function seedanceUsageSchema(profile) {
  return {
    tokens: { type: "number", unit: "token", description: { en: "Video generation token unit price", zh: "视频生成 Token 单价" } },
    resolution: {
      enum: profile.resolutions,
      enumLabels: Object.fromEntries(profile.resolutions.map((resolution) => [resolution, { en: resolution, zh: resolution }])),
      description: { en: "Output video resolution", zh: "输出视频分辨率" },
    },
    video_input: {
      enum: ["none", "video"],
      enumLabels: { none: { en: "No reference video", zh: "无参考视频" }, video: { en: "With reference video", zh: "有参考视频" } },
      description: { en: "Reference video input", zh: "参考视频输入" },
    },
  };
}

// 示例只展示用量估算，不内置 ZZEngine 价格；正式结算使用实测 Token。
function seedanceUsageExamples(profile) {
  const specs = profile.resolutions.map((resolution) => ({ label: resolution + " · 5s", resolution: resolution, seconds: 5 }));
  specs.push({ label: "720p · 5s (+4s 输入视频)", resolution: "720p", seconds: 9, video_input: "video" });
  return specs.map((spec) => ({
    label: spec.label,
    facts: { tokens: Math.round(estimateTokens(spec.seconds, spec.resolution)), resolution: spec.resolution, video_input: spec.video_input || "none" },
  }));
}

function capabilityUsageProfiles(models, usageSchema, usageExamples) {
  const profiles = [];
  for (const model of Object.keys(models)) {
    const schema = usageSchema(models[model]);
    const examples = usageExamples(models[model]);
    const shape = JSON.stringify([schema, examples]);
    let profile = profiles.find((entry) => entry.shape === shape);
    if (!profile) {
      profile = { shape: shape, models: [], schema: schema, examples: examples };
      profiles.push(profile);
    }
    profile.models.push(model);
  }
  return profiles.map((profile) => ({ models: profile.models, schema: profile.schema, examples: profile.examples }));
}

export const meta = {
  apiVersion: 1,
  key: "zzengine",
  name: "Doubao Video（ZZEngine）",
  icon: "Doubao.Color",
  description: { en: "Seedance video generation through ZZEngine", zh: "通过 ZZEngine 生成 Seedance 视频" },
  version: "1.0.2",
  requiredCapabilities: ["json-clone@1", "task-callback@1", "shared-native-routes@1", "poll-task-data@1"],
  author: { name: "QuantumNous" },
  baseUrl: "https://model-relay-api.zzengine.net",
  website: "https://model-relay.zzengine.net",
  models: Object.keys(VIDEO_MODELS),
  fetchMode: "per_task",
  upstreams: ["vendor", "new_api"],
  usageSchema: seedanceUsageSchema(DEFAULT_VIDEO_PROFILE),
  usageExamples: seedanceUsageExamples(DEFAULT_VIDEO_PROFILE),
  usageProfiles: capabilityUsageProfiles(VIDEO_MODELS, seedanceUsageSchema, seedanceUsageExamples),
  routes: [
    { method: "POST", path: "/doubao/api/v3/contents/generations/tasks", protocol: "seedance-v3", type: "submit", decode: "createTask", render: "taskCreated" },
    { method: "GET", path: "/doubao/api/v3/contents/generations/tasks/:task_id", protocol: "seedance-v3", type: "query", render: "taskStatus" },
    {
      method: "GET",
      path: "/doubao/api/v3/contents/generations/tasks",
      protocol: "seedance-v3",
      type: "dynamic",
      action: "list",
      decode: "listTasks",
      render: "taskList",
    },
    {
      method: "DELETE",
      path: "/doubao/api/v3/contents/generations/tasks/:task_id",
      protocol: "seedance-v3",
      type: "dynamic",
      action: "delete",
      decode: "deleteTask",
      render: "taskDeleted",
    },
  ],
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};

function trimmed(value) {
  return String(value || "").trim();
}

// 服务前缀由管理员在渠道 BaseURL 中配置。
function apiRoot(ctx) {
  return trimmed(ctx.baseUrl).replace(/\/+$/, "");
}

function objectValue(value, name) {
  if (value === undefined || value === null) return {};
  if (typeof value !== "object" || Array.isArray(value)) throw new Error(name + " must be an object");
  return value;
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
    return { ...item, draft_task: { ...draft, id: upstream } };
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

function videoProfileForModel(model, declaredModel) {
  for (const candidate of [model, declaredModel]) {
    const key = modelKey(candidate);
    if (Object.prototype.hasOwnProperty.call(VIDEO_MODELS, key)) return VIDEO_MODELS[key];
  }
  return null;
}

// 渠道接入点 ID 的能力由已声明模型决定。
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
      lastFrameRequiresFirst: true,
    };
  }
  if (SEEDANCE_25_MODELS.includes(name)) {
    const isFlash = SEEDANCE_25_FLASH_MODELS.includes(name);
    return {
      minDuration: 4,
      maxDuration: 30,
      autoDuration: true,
      frames: false,
      serviceTier: false,
      draft: !isFlash,
      framesInput: true,
      lastFrame: true,
      reference: true,
      adaptiveRatio: true,
      generateAudio: true,
      priority: true,
      referenceImages: isFlash ? 10 : 30,
      referenceVideos: isFlash ? 5 : 10,
      referenceAudios: isFlash ? 5 : 10,
      audioOnly: true,
      lastFrameRequiresFirst: isFlash,
    };
  }
  return null;
}

function validationModel(ctx, model) {
  for (const candidate of [model, ctx.declaredModel, ctx.model]) {
    if (modelCapabilities(candidate)) return candidate;
  }
  return model;
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
function requestResolution(model, req, declaredModel) {
  const resolutionField = requestField(req, "resolution");
  const sizeField = requestField(req, "size");
  const rawValue = resolutionField.present ? resolutionField.value : sizeField.present ? sizeField.value : "";
  // 错误类型不能经trimmed的真假值处理变成缺省分辨率。
  if (rawValue !== undefined && rawValue !== null && typeof rawValue !== "string") throw new Error("resolution or size must be a string");
  const raw = trimmed(rawValue).toLowerCase();
  // 已知上游模型优先；未声明的接入点 ID 才回退到公有模型能力。
  const capability = videoProfileForModel(model, declaredModel);
  const available = capability ? capability.resolutions : SEEDANCE_RESOLUTIONS;
  if (!raw) return available.includes("1080p") ? "1080p" : available[available.length - 1];
  const recognized = ["480p", "720p", "1080p", "4k"].includes(raw) || /^\d+[x*]\d+$/.test(raw);
  if (!recognized) throw new Error("resolution must be 480p, 720p, 1080p, or 4k");
  const resolution = normalizeResolution(raw);
  if (!available.includes(resolution)) throw new Error((trimmed(declaredModel) || model) + " resolution must be one of " + available.join(", "));
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
    const draftField = requestField(req, "draft");
    if (draftField.present && draftField.value === true && serviceTier === "flex") throw new Error("draft tasks do not support flex service_tier");
    if (capabilities && !capabilities.serviceTier && serviceTier !== "default") throw new Error(model + " does not support service_tier");
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
  if (capabilities && capabilities.lastFrameRequiresFirst && lastFrames > 0 && firstFrames === 0) throw new Error("last_frame requires first_frame");
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
    videoCount: videoCount,
  };
}

// 校验官方顶层选项，防止兼容接口把明显无效的类型和范围交给上游。
function validateDoubaoOptions(model, req, contentInfo, strictTypes) {
  const capabilities = modelCapabilities(model);
  const outputFormat = requestField(req, "output_format");
  if (outputFormat.present && !["mp4", "mov"].includes(outputFormat.value)) throw new Error("output_format must be mp4 or mov");
  if (SEEDANCE_20_MODELS.includes(modelKey(model)) && outputFormat.present && outputFormat.value !== "mp4")
    throw new Error(model + " only supports mp4 output_format");
  // 回调只校验地址结构，实际投递由宿主限制为公网 HTTPS；同步请求不做网络探测。
  const callback = requestField(req, "callback_url");
  if (callback.present) {
    const address =
      typeof callback.value === "string" && callback.value.length <= 4096
        ? callback.value.match(/^https:\/\/([a-z0-9.-]+)(?::([0-9]{1,5}))?(?:[/?][^\s#\\]*)?$/i)
        : null;
    if (
      !address ||
      !address[1].includes(".") ||
      /^[0-9.]+$/.test(address[1]) ||
      /(?:^|\.)localhost\.?$/i.test(address[1]) ||
      (address[2] && (Number(address[2]) < 1 || Number(address[2]) > 65535))
    )
      throw new Error("callback_url must be a public HTTPS domain URL");
  }
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
    // 2.0 只兼容 SDK 的 false 默认值，开启固定镜头仍应拒绝。
    if (key === "camera_fixed" && field.present && SEEDANCE_20_MODELS.includes(modelKey(model)) && field.value !== false)
      throw new Error(model + " does not support camera_fixed");
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
  const moderation = requestField(req, "moderation_options");
  if (moderation.present) objectValue(moderation.value, "moderation_options");
  if (SEEDANCE_25_MODELS.includes(modelKey(model))) {
    const taskType = requestField(req, "omni_reference_task_type");
    if (taskType.present && !["auto", "reference", "edit", "extend"].includes(taskType.value))
      throw new Error("omni_reference_task_type must be auto, reference, edit, or extend");
    const ratio = requestField(req, "ratio");
    const requiresAdaptive = (contentInfo && contentInfo.hasFrames) || taskType.value === "edit" || taskType.value === "extend";
    if (requiresAdaptive && ratio.present && trimmed(ratio.value) && trimmed(ratio.value) !== "adaptive")
      throw new Error("Seedance 2.5 frame images, edit, and extend require adaptive ratio");
    if (taskType.value === "edit" || taskType.value === "extend") {
      if (!contentInfo || !contentInfo.hasVideo) throw new Error("edit and extend require reference video input");
      if (taskType.value === "extend" && contentInfo.videoCount > 3) throw new Error("extend accepts at most 3 reference videos");
      const seconds = requestField(req, "seconds");
      const duration = requestField(req, "duration");
      if (taskType.value === "edit" && (seconds.present || duration.present) && Number(seconds.present ? seconds.value : duration.value) !== -1)
        throw new Error("edit requires automatic duration");
    }
  }
}

function hasVideo(content) {
  return Array.isArray(content) && content.some((item) => item && (item.type === "video_url" || Object.prototype.hasOwnProperty.call(item, "video_url")));
}

// 样片引用沿用已验证原任务的输入类型，无法确认时不能猜测计价档位。
function videoInputKind(ctx, content) {
  const ids = draftTaskIds(content);
  if (!ids.length) return hasVideo(content) ? "video" : "none";
  for (const id of ids) {
    const origin = Array.isArray(ctx.originTasks) && ctx.originTasks.find((task) => task && task.taskId === id);
    if (!origin || !["none", "video"].includes(origin.videoInput)) throw new Error("draft task input type is unavailable");
    if (origin.videoInput === "video") return "video";
  }
  return "none";
}

function videoProfile(ctx) {
  const profile = videoProfileForModel(ctx && ctx.upstreamModel, ctx && (ctx.declaredModel || ctx.model));
  if (profile) return profile;
  return DEFAULT_VIDEO_PROFILE;
}

// 使用各比例中的最大像素数预扣，最终按上游实际 token 结算。
function resolutionMaxPixels(resolution) {
  if (resolution === "480p") return [864, 496];
  if (resolution === "1080p") return [1920, 1088];
  if (resolution === "4k") return [3326, 2494];
  return [1504, 640];
}

function estimateTokens(seconds, resolution) {
  const dims = resolutionMaxPixels(resolution);
  return (seconds * dims[0] * dims[1] * VIDEO_FPS) / 1024;
}

// 明确的零用量与缺失不同；错误类型返回 null，禁止 JS 隐式转换造成低额结算。
function billingTokenCount(value) {
  if ((typeof value !== "number" && typeof value !== "string") || String(value).trim() === "") return null;
  const tokens = Number(value);
  return Number.isSafeInteger(tokens) && tokens >= 0 ? tokens : null;
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
  const output = { ...data, id: task.task_id };
  const statusMap = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "running", SUCCESS: "succeeded", FAILURE: "failed" };
  output.status = statusMap[task.status] || "queued";
  if (task.fail_reason === "task cancelled by user") output.status = "cancelled";
  else if (task.status === "FAILURE" && data.status === "expired") output.status = "expired";
  if (!output.created_at && task.created_at) output.created_at = task.created_at;
  if (!output.updated_at && task.updated_at) output.updated_at = task.updated_at;
  if (task.fail_reason && output.status === "failed" && !output.error) output.error = { code: "task_failed", message: task.fail_reason };
  if (output.status === "succeeded") output.error = null;
  return output;
}

export const native = {
  createTask: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
    const input = ctx.body.value;
    if (!input || typeof input !== "object" || Array.isArray(input)) throw new Error("request body must be an object");
    const body = utils.json.clone(input);
    // 方舟 SDK 将未设置的可选字段发送为 null，按未提供处理。
    for (const key of [
      "safety_identifier",
      "callback_url",
      "return_last_frame",
      "service_tier",
      "execution_expires_after",
      "priority",
      "generate_audio",
      "draft",
      "camera_fixed",
      "watermark",
      "seed",
      "resolution",
      "ratio",
      "duration",
      "frames",
      "tools",
      "output_format",
      "omni_reference_task_type",
      "moderation_options",
    ]) {
      if (body[key] === null) delete body[key];
    }
    const model = trimmed(body.model);
    if (!model) throw new Error("model is required");
    requestResolution(model, body, model);
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
    if (!["", "queued", "running", "cancelled", "succeeded", "failed", "expired"].includes(status)) throw new Error("filter.status is invalid");
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
  const capabilityModel = validationModel(ctx, model);
  requestResolution(model, req, ctx.declaredModel || ctx.model || req.model);
  validateGenerationParameters(req, capabilityModel, false);
  const metadata = objectValue(req.metadata, "metadata");
  const body = { model: req.model || "", content: [], ...metadata };
  // Responses/OpenAI 兼容层把官方选项放在顶层，这些字段必须进入最终上游请求。
  for (const key of DOUBAO_COMPATIBILITY_FIELDS) {
    if (Object.prototype.hasOwnProperty.call(req, key)) body[key] = req[key];
  }
  // 原生 content 的顺序、兼容接口图片和素材引用都必须原样保留。
  body.content = requestContent(req, true);
  videoInputKind(ctx, body.content);
  if (Array.isArray(body.content)) body.content = rewriteDraftTaskContent(body.content, ctx.originTasks);
  // duration 别名和智能时长 -1 都必须真正发送；不能截断小数后丢失请求数量。
  const secondsField = requestField(req, "seconds");
  const durationField = requestField(req, "duration");
  if (secondsField.present || durationField.present) body.duration = Number(secondsField.present ? secondsField.value : durationField.value);
  delete body.seconds;
  for (const key of ["frames", "seed", "priority", "execution_expires_after"]) {
    const field = requestField(req, key);
    if (field.present) body[key] = integerFieldValue(field.value, true);
  }
  // 上游型号统一使用渠道模型映射结果。
  body.model = model;
  // 渠道别名映射后再次校验；兼容接口的 size/resolution 必须与实际发往上游的值一致。
  const resolutionField = requestField(req, "resolution");
  const sizeField = requestField(req, "size");
  if ((resolutionField.present && trimmed(resolutionField.value)) || (sizeField.present && trimmed(sizeField.value)))
    body.resolution = requestResolution(model, req, ctx.declaredModel || ctx.model || req.model);
  const contentInfo = validateDoubaoContent(capabilityModel, body.content);
  validateDoubaoOptions(capabilityModel, body, contentInfo, false);
  const callbackURL = body.callback_url;
  delete body.callback_url;
  // 用户标识由本站保存并回显，不发送给上游。
  delete body.safety_identifier;
  const hasReference = contentInfo.hasMedia;
  return {
    url: apiRoot(ctx) + "/api/v3/contents/generations/tasks",
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: "Bearer " + ctx.apiKey },
    body: body,
    action: hasReference ? "image_to_video" : "text_to_video",
    rewriteModel: body.model,
    callback: callbackURL === undefined ? undefined : { url: callbackURL, bodyField: "callback_url" },
  };
}

export function parseSubmitResponse(ctx, resp) {
  if (
    !resp.body ||
    typeof resp.body !== "object" ||
    Array.isArray(resp.body) ||
    typeof resp.body.id !== "string" ||
    !trimmed(resp.body.id) ||
    resp.body.id.length > 191
  )
    throw new Error("task_id is empty or invalid");
  const req = ctx.requestBody || {};
  const state = {};
  const content = requestContent(req, false);
  // 受理恢复可能没有原任务上下文，不阻止已受理任务重新进入轮询。
  if (!draftTaskIds(content).length || Array.isArray(ctx.originTasks)) state.video_input = videoInputKind(ctx, content);
  const safetyIdentifier = requestField(req, "safety_identifier");
  if (safetyIdentifier.present) state.safety_identifier = safetyIdentifier.value;
  // 用户标识从私有状态回显，其他结果字段仍取上游响应。
  const taskData = utils.json.clone(resp.body);
  const serviceTier = requestField(req, "service_tier");
  delete taskData._seedance_inferred_service_tier;
  if (!Object.prototype.hasOwnProperty.call(taskData, "service_tier")) {
    taskData.service_tier = serviceTier.value || "default";
    taskData._seedance_inferred_service_tier = true;
  }
  return { taskId: trimmed(resp.body.id), taskData: taskData, state: state };
}

export function extractUsage(ctx) {
  const req = ctx.requestBody || {};
  const model = ctx.upstreamModel || ctx.model || req.model || "";
  const capabilityModel = validationModel(ctx, model);
  validateGenerationParameters(req, capabilityModel, false);
  const content = requestContent(req, false);
  const contentInfo = content.length ? validateDoubaoContent(capabilityModel, content) : { hasVisual: false };
  validateDoubaoOptions(capabilityModel, req, contentInfo, false);
  const resolution = requestResolution(model, req, ctx.declaredModel || ctx.model || req.model);
  const inputKind = videoInputKind(ctx, content);
  const billingModel =
    [model, ctx.declaredModel, ctx.model, req.model].map(modelKey).find((name) => Object.prototype.hasOwnProperty.call(VIDEO_MODELS, name)) || modelKey(model);
  // 不沿用火山单价倍率；ZZEngine 各档价格由管理员单独配置。
  if (ctx.usagePurpose === "billing_ratios") return null;
  // 官方 frames 优先于 duration；非整数秒不能向下取整后少预扣。
  const framesField = requestField(req, "frames");
  const frames = framesField.present ? integerFieldValue(framesField.value, true) : null;
  const secondsField = requestField(req, "seconds");
  const durationField = requestField(req, "duration");
  const durationValue = secondsField.present ? secondsField.value : durationField.value;
  let seconds = frames !== null && frames > 0 ? frames / 24 : Number(durationValue || 0);
  if (!Number.isFinite(seconds) || seconds <= 0) {
    seconds = SEEDANCE_25_MODELS.includes(billingModel) ? 30 : 15;
  }
  const inputSeconds = inputKind === "video" ? (SEEDANCE_25_MODELS.includes(billingModel) ? 30 : 15) : 0;
  const outputFrames = frames !== null && frames > 0 ? frames : seconds * VIDEO_FPS + VIDEO_RESERVE_EXTRA_FRAMES;
  const dims = resolutionMaxPixels(resolution);
  const facts = { tokens: Math.ceil(((outputFrames + inputSeconds * VIDEO_FPS) * dims[0] * dims[1]) / 1024), seconds: seconds, resolution: resolution };
  facts.video_input = inputKind;
  return facts;
}

export function buildQueryRequest(ctx) {
  return {
    url: apiRoot(ctx) + "/api/v3/contents/generations/tasks/" + encodeURIComponent(ctx.taskId),
    method: "GET",
    headers: { Accept: "application/json", "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey },
  };
}

// 管理操作继续使用渠道密钥和真实上游任务 ID，客户端只持有网关公开 ID。
export function buildTaskActionRequest(ctx) {
  if (ctx.operation !== "delete") throw new Error("unsupported task operation");
  return {
    url: apiRoot(ctx) + "/api/v3/contents/generations/tasks/" + encodeURIComponent(ctx.taskId),
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
  if (!body || typeof body !== "object" || Array.isArray(body)) return { status: "UNKNOWN", reason: "invalid task response" };
  if (body.id !== undefined && body.id !== ctx.taskId) return { status: "UNKNOWN", reason: "task response id does not match requested task" };
  if (body.status === "pending" || body.status === "queued") return { status: "QUEUED", progress: "10%", taskData: body };
  if (body.status === "processing" || body.status === "running") return { status: "IN_PROGRESS", progress: "50%", taskData: body };
  if (body.status === "succeeded") {
    const result = { status: "SUCCESS", progress: "100%", taskData: body, url: body.content && body.content.video_url ? body.content.video_url : "" };
    const usage = body.usage || {};
    const completionTokens = billingTokenCount(usage.completion_tokens);
    const totalTokens = billingTokenCount(usage.total_tokens);
    if (Number.isFinite(completionTokens) && completionTokens > 0) result.completionTokens = completionTokens;
    if (Number.isFinite(totalTokens) && totalTokens > 0) result.totalTokens = totalTokens;
    return result;
  }
  if (body.status === "failed" || body.status === "expired" || body.status === "cancelled") {
    const error = body.error && typeof body.error === "object" ? publicFailureValue(ctx, body.error) : null;
    let fallback = "";
    for (const field of ["failure_reason", "reason", "message", "error"]) {
      if (typeof body[field] === "string" && body[field]) {
        fallback = body[field];
        break;
      }
    }
    const message = error && typeof error.message === "string" ? error.message : publicFailureValue(ctx, fallback);
    const reason = body.status === "cancelled" ? "task cancelled by user" : message || body.status;
    const taskData = utils.json.clone(body);
    if (body.error !== undefined) taskData.error = error || publicFailureValue(ctx, body.error);
    for (const key of ["message", "reason", "failure_reason"]) {
      if (taskData[key] !== undefined) taskData[key] = publicFailureValue(ctx, taskData[key]);
    }
    return { status: "FAILURE", progress: "100%", reason: reason, taskData: taskData };
  }
  return { status: "UNKNOWN", reason: "unrecognized status: " + String(body.status || "") };
}

// 错误详情保留业务原因与代码，只清理渠道身份、凭据和内部任务标识。
function publicFailureValue(ctx, value) {
  if (Array.isArray(value)) return value.map((item) => publicFailureValue(ctx, item));
  if (value && typeof value === "object") {
    const result = {};
    for (const key of Object.keys(value)) {
      const name = key.replace(/[^a-z0-9]/gi, "").toLowerCase();
      if (
        /^(internal|upstream|provider)/.test(name) ||
        [
          "proto", "prototype", "constructor", "apikey", "secretkey", "token", "accesstoken", "refreshtoken", "secret", "password", "authorization",
          "credentials", "headers", "cookie", "stack", "requestid", "traceid", "channel", "channelid", "channelname",
        ].includes(name)
      )
        continue;
      result[key] = publicFailureValue(ctx, value[key]);
    }
    return result;
  }
  if (typeof value !== "string") return value;
  let message = value;
  const key = String(ctx.apiKey || "");
  const privateId = String(ctx.taskId || "");
  const upstreamModel = String(ctx.upstreamModel || "");
  if (key) message = message.split(key).join("[redacted]");
  if (privateId && privateId !== ctx.publicTaskId) message = message.split(privateId).join(ctx.publicTaskId || "[redacted]");
  if (upstreamModel && upstreamModel !== ctx.model) message = message.split(upstreamModel).join(ctx.model || "[redacted]");
  message = message.replace(/https?:\/\/[^\s<>]+/gi, "[redacted URL]");
  const hostMatch = String(ctx.baseUrl || "").match(/^https?:\/\/(?:[^@/]*@)?([^/:?#]+)/i);
  if (hostMatch) {
    const host = hostMatch[1];
    const labels = host.split(".");
    const brand = labels.find((label) => label.length >= 4 && !["www", "api", "openapi", "cloud", "video", "gateway", "volces", "volcengine"].includes(label.toLowerCase()));
    for (const name of [host, brand]) {
      if (!name) continue;
      const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      message = message.replace(new RegExp(escaped, "gi"), "[redacted]");
    }
  }
  return message
    .replace(/\bzzengine\b/gi, "[redacted]")
    .replace(/\b(?:bearer\s+|sk-)[^\s,;<>]+/gi, "[redacted]")
    .replace(/\b(?:api[_-]?key|token|secret|authorization)\s*[:=]\s*[^\s,;<>]+/gi, "[redacted]")
    .replace(/[\x00-\x1f\x7f]/g, " ")
    .trim()
    .slice(0, 2048);
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  const data = task.data || {};
  const content = data.content || {};
  const artifacts = [];
  if (trimmed(content.video_url)) {
    const artifact = { key: "video", type: "video" };
    if (data.output_format === "mp4") artifact.mimeType = "video/mp4";
    else if (data.output_format === "mov") artifact.mimeType = "video/quicktime";
    artifacts.push(artifact);
  }
  if (trimmed(content.first_frame_url)) artifacts.push({ key: "first_frame", type: "image" });
  if (trimmed(content.last_frame_url)) artifacts.push({ key: "last_frame", type: "image" });
  return artifacts;
}

export function buildContentRequest(ctx) {
  const content = (ctx.data && ctx.data.content) || {};
  const urls = { video: content.video_url, first_frame: content.first_frame_url, last_frame: content.last_frame_url };
  const url = trimmed(urls[ctx.artifactKey]);
  if (!url) throw new Error("artifact_not_found");
  // 上游媒体地址已签名，不能把渠道密钥发送到媒体站点。
  return { url: url, method: ctx.artifactKey === "video" ? ctx.clientRequest.method : "GET", credentialless: true };
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
  // 结算秒数只取成功响应的实际时长，不回退请求时长或智能时长预算。
  if (typeof body.duration === "number" && Number.isFinite(body.duration) && body.duration > 0 && body.duration <= 3600) facts.seconds = body.duration;
  const content = body.content || {};
  const resolution = trimmed(content.resolution || body.resolution).toLowerCase();
  if (videoProfile(task).resolutions.includes(resolution)) facts.resolution = resolution;
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
      const input = responsesInput(req);
      if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata)))
        throw new Error("metadata must be an object");
      const prompt = input.prompt || trimmed(req.prompt);
      const images = [];
      for (const image of [req.image, req.input_reference].concat(req.images || [], input.images)) {
        if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
      }
      const effectiveModel = ctx.upstreamModel || model;
      const metadata = utils.json.clone(req.metadata || {});
      if (Object.prototype.hasOwnProperty.call(req, "resolution")) metadata.resolution = req.resolution;
      else if (req.size && !metadata.resolution) metadata.resolution = requestResolution(effectiveModel, { size: req.size }, model);
      const requestBody = { model: model, prompt: prompt, metadata: metadata };
      for (const key of DOUBAO_COMPATIBILITY_FIELDS) {
        if (Object.prototype.hasOwnProperty.call(req, key)) requestBody[key] = req[key];
      }
      if (images.length) requestBody.images = images;
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.seconds = req.seconds;
      if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.duration = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      requestResolution(effectiveModel, requestBody, model);
      const capabilityModel = validationModel(ctx, effectiveModel);
      validateGenerationParameters(requestBody, capabilityModel, false);
      const content = requestContent(requestBody, true);
      const contentInfo = validateDoubaoContent(capabilityModel, content);
      validateDoubaoOptions(capabilityModel, requestBody, contentInfo, false);
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
      if (status === "FAILURE") {
        const error = task.data && task.data.error;
        return { events: [{ type: "error", code: error && error.code ? String(error.code) : "task_failed", message: task.fail_reason || "task failed" }], state: state, done: true };
      }
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

protocols.openai_video = {
  decodeRequest: function (ctx) {
    if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
    if (ctx.body.kind === "json") {
      if (!ctx.body.value || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
      const req = utils.json.clone(ctx.body.value);
      const model = ctx.upstreamModel || ctx.model;
      requestResolution(model, req, ctx.model);
      const capabilityModel = validationModel(ctx, model);
      validateGenerationParameters(req, capabilityModel, false);
      const content = requestContent(req, true);
      const contentInfo = validateDoubaoContent(capabilityModel, content);
      validateDoubaoOptions(capabilityModel, req, contentInfo, false);
      const intent = {
        kind: "submit",
        model: ctx.model,
        action: contentInfo.hasMedia ? "image_to_video" : "text_to_video",
        requestBody: { ...req, model: ctx.model },
      };
      const originTaskIds = draftTaskIds(content);
      if (originTaskIds.length) intent.originTaskIds = originTaskIds;
      return intent;
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
    for (const key of ["metadata", "moderation_options"]) {
      if (req[key] === undefined) continue;
      let parsed;
      try {
        parsed = JSON.parse(req[key]);
      } catch (e) {
        throw new Error(key + " must be a JSON object string", { cause: e });
      }
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error(key + " must be a JSON object string");
      req[key] = parsed;
    }
    for (const key of ["content", "images", "tools"]) {
      if (req[key] === undefined) continue;
      let parsed;
      try {
        parsed = JSON.parse(req[key]);
      } catch (e) {
        throw new Error(key + " must be a JSON array string", { cause: e });
      }
      if (!Array.isArray(parsed)) throw new Error(key + " must be a JSON array string");
      req[key] = parsed;
    }
    if ((ctx.body.files || []).length) throw new Error("Image and video references must be URLs inside metadata.content");
    const model = ctx.upstreamModel || ctx.model;
    requestResolution(model, req, ctx.model);
    const capabilityModel = validationModel(ctx, model);
    validateGenerationParameters(req, capabilityModel, false);
    if (req.seconds !== undefined) req.seconds = Number(req.seconds);
    else if (req.duration !== undefined) req.seconds = Number(req.duration);
    const content = requestContent(req, true);
    const contentInfo = validateDoubaoContent(capabilityModel, content);
    validateDoubaoOptions(capabilityModel, req, contentInfo, false);
    const intent = {
      kind: "submit",
      model: ctx.model,
      action: contentInfo.hasMedia ? "image_to_video" : "text_to_video",
      requestBody: { ...req, model: ctx.model },
    };
    const originTaskIds = draftTaskIds(content);
    if (originTaskIds.length) intent.originTaskIds = originTaskIds;
    return intent;
  },
  render: function (ctx, task) {
    return nativeTask(task);
  },
};
