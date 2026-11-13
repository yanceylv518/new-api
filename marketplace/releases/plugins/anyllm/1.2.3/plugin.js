const MODELS = {
  "doubao-seedance-2-0-260128": ["480p", "720p", "1080p", "4k"],
  "doubao-seedance-2-0-fast-260128": ["480p", "720p"],
  "doubao-seedance-2-0-mini-260615": ["480p", "720p"],
  "doubao-seedance-2-5-260628": ["480p", "720p", "1080p"],
};
const IMAGE_MODELS = {
  "doubao-seedream-5-0-pro-260628": {
    presets: ["1K", "1.5K", "2K"],
    minPixels: 921600,
    maxPixels: 4624220,
    maxReferenceImages: 10,
    sequential: false,
    layers: true,
    outputFormat: true,
    background: true,
    tools: false,
    fastPromptMode: true,
  },
  "doubao-seedream-5-0-flash-260915": {
    presets: ["1K", "1.5K", "2K"],
    minPixels: 921600,
    maxPixels: 4624220,
    maxReferenceImages: 10,
    sequential: false,
    layers: true,
    outputFormat: true,
    background: true,
    tools: false,
    fastPromptMode: false,
  },
};

const IMAGE_RESOLUTIONS = ["1K", "1.5K", "2K", "3K", "4K"];
const IMAGE_PRESET_PIXELS = { "1K": 1048576, "1.5K": 2359296, "2K": 4194304, "3K": 9437184, "4K": 16777216 };
const DEFAULT_IMAGE_PROFILE = {
  presets: IMAGE_RESOLUTIONS,
  minPixels: 921600,
  maxPixels: 16777216,
  maxReferenceImages: 14,
  sequential: true,
  layers: true,
  outputFormat: true,
  background: true,
  tools: true,
  fastPromptMode: true,
};
const LAYER_SIZE_PRESETS = ["1K", "1.5K", "2K"];
const IMAGE_ACTIONS = ["text_to_image", "image_to_image"];
const IMAGE_TIER_MAX_PIXELS = 2610000;
const MAX_IMAGE_OUTPUTS = 17;
const MAX_REFERENCE_IMAGES = 14;
const MAX_INPUT_IMAGE_BYTES = 31457280;
const IMAGE_DATA_URI = /^data:image\/[a-z0-9.+-]+;base64,[A-Za-z0-9+/]+={0,2}$/;
const IMAGE_REQUEST_KEYS = [
  "size",
  "seed",
  "guidance_scale",
  "response_format",
  "watermark",
  "optimize_prompt",
  "optimize_prompt_options",
  "sequential_image_generation",
  "sequential_image_generation_options",
  "output_format",
  "layer_decomposition",
];

const IMAGE_UNIT_LABEL = { en: "image", zh: "张", "zh-TW": "張", fr: "image", ja: "枚", ru: "изображение", vi: "ảnh" };
const IMAGE_USAGE_SCHEMA = {
  images_up_to_1_5k: {
    type: "number",
    displayOrder: 10,
    unit: "count",
    unitLabel: IMAGE_UNIT_LABEL,
    description: { en: "Image generation unit price (1.5K and below)", zh: "图片生成单价（1.5K 及以下）" },
  },
  images_above_1_5k: {
    type: "number",
    displayOrder: 20,
    unit: "count",
    unitLabel: IMAGE_UNIT_LABEL,
    description: { en: "Image generation unit price (above 1.5K)", zh: "图片生成单价（1.5K 以上）" },
  },
  input_images: {
    type: "number",
    displayOrder: 30,
    unit: "count",
    unitLabel: IMAGE_UNIT_LABEL,
    description: { en: "Input image unit price", zh: "输入图片单价" },
  },
  layer_decomposition: {
    type: "boolean",
    description: { en: "Whether layer decomposition is enabled", zh: "是否开启图层拆分" },
  },
};

function hasLowerImageTier(profile) {
  return profile.minPixels <= IMAGE_TIER_MAX_PIXELS;
}

function seedreamUsageFacts(lower, higher, inputImages, layered) {
  return {
    images_up_to_1_5k: lower,
    images_above_1_5k: higher,
    input_images: inputImages,
    layer_decomposition: layered,
    // 宿主使用实际张数更新数量折扣和日志，不新增计价列。
    image_count: lower + higher,
  };
}

function seedreamUsageExamples(profile) {
  const examples = [{ label: "2K · 1 张", facts: seedreamUsageFacts(0, 1, 0, false) }];
  if (hasLowerImageTier(profile)) examples.push({ label: "1K · 1 张 · 2 张参考图", facts: seedreamUsageFacts(1, 0, 2, false) });
  else examples.push({ label: "2K · 1 张 · 2 张参考图", facts: seedreamUsageFacts(0, 1, 2, false) });
  if (profile.sequential) examples.push({ label: "2K · 4 张组图", facts: seedreamUsageFacts(0, 4, 0, false) });
  if (profile.layers) examples.push({ label: "图层拆分 · 2K 底图 + 4 层 1.5K", facts: seedreamUsageFacts(4, 1, 1, true) });
  const keys = Object.keys(IMAGE_USAGE_SCHEMA);
  return examples.map((example) => ({ label: example.label, facts: Object.fromEntries(keys.map((key) => [key, example.facts[key]])) }));
}

const OPTIONS = [
  "seed",
  "omni_reference_task_type",
  "ratio",
  "generate_audio",
  "watermark",
  "output_format",
  "return_last_frame",
  "service_tier",
  "draft",
  "execution_expires_after",
  "priority",
  "tools",
];
const PIXELS = { "480p": 864 * 480, "720p": 1280 * 720, "1080p": 1920 * 1080, "4k": 3840 * 2160 };

function schema(resolutions) {
  return {
    tokens: { type: "number", unit: "token", description: { en: "Billing token unit price", zh: "计费 Token 单价" } },
    resolution: { enum: resolutions, description: { en: "Output video resolution", zh: "输出视频分辨率" } },
    video_input: {
      enum: ["none", "video"],
      enumLabels: { none: { en: "No reference video", zh: "无参考视频" }, video: { en: "With reference video", zh: "有参考视频" } },
      description: { en: "Reference video input", zh: "参考视频输入" },
    },
  };
}

function examples(resolutions) {
  return resolutions.map((resolution) => ({
    label: resolution + " · 5s",
    facts: { tokens: Math.ceil((5 * PIXELS[resolution] * 24) / 1024), resolution, video_input: "none" },
  }));
}

export const meta = {
  apiVersion: 1,
  key: "anyllm",
  name: "Doubao Video（AnyLLM）",
  icon: "text:SD",
  version: "1.2.3",
  author: { name: "PinduCloud" },
  description: { en: "Seedance video generation, Seedream image generation and private assets", zh: "Seedance 视频生成、Seedream 图片生成与私域素材" },
  requiredCapabilities: ["shared-native-routes@1", "poll-task-data@1"],
  models: Object.keys(MODELS).concat(Object.keys(IMAGE_MODELS)),
  fetchMode: "per_task",
  usageSchema: schema(["480p", "720p", "1080p", "4k"]),
  usageExamples: examples(["480p", "720p", "1080p", "4k"]),
  usageProfiles: Object.keys(MODELS)
    .map((model) => ({ models: [model], schema: schema(MODELS[model]), examples: examples(MODELS[model]) }))
    .concat({ models: Object.keys(IMAGE_MODELS), schema: IMAGE_USAGE_SCHEMA, examples: seedreamUsageExamples(IMAGE_MODELS["doubao-seedream-5-0-pro-260628"]) }),
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
      method: "POST",
      path: "/doubao/api/v3/images/generations",
      protocol: "seedream-v3",
      models: Object.keys(IMAGE_MODELS),
      type: "submit",
      decode: "createImage",
      render: "imageCreated",
      retainResult: false,
    },
  ],
  protocols: [
    { name: "openai_responses", supports: ["stream", "sync", "background"] },
    { name: "openai_video", models: Object.keys(MODELS) },
    { name: "openai_image", models: Object.keys(IMAGE_MODELS) },
  ],
};

function object(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(label + " must be an object");
  return value;
}

function text(value) {
  return typeof value === "string" ? value.trim() : "";
}

function diagnosticIdentifier(value, limit, apiKey) {
  const identifier = text(value);
  if (!identifier || identifier.length > limit || !/^[a-zA-Z0-9_.-]+$/.test(identifier) || /^sk-/i.test(identifier)) return "";
  if (text(apiKey) && identifier.includes(apiKey)) return "";
  return identifier;
}

function integer(value, min, max, label) {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < min || value > max) throw new Error(label + " is out of range");
  return value;
}

function apiRoot(ctx) {
  const base = text(ctx.baseUrl).replace(/\/+$/, "");
  return /\/v1$/i.test(base) ? base : base + "/v1";
}

function mediaURL(value) {
  if (typeof value !== "string" || !/^https?:\/\/[^\s]+$/i.test(value)) throw new Error("Reference media must use a public HTTP(S) URL");
  return value;
}

function referenceURL(value) {
  // asset:// 的归属与目标账号映射由宿主统一完成；下载仍只允许公网 URL。
  if (typeof value === "string" && /^asset:\/\/[a-zA-Z0-9_-]{1,128}$/i.test(value)) return value;
  return mediaURL(value);
}

function modelProfile(ctx, requestedModel) {
  const model = text(ctx.upstreamModel) || text(ctx.model) || text(requestedModel);
  if (MODELS[model]) return model;
  if (MODELS[ctx.model]) return ctx.model;
  throw new Error("Unsupported Seedance video model");
}

function trimmed(value) {
  return String(value || "").trim();
}

function objectValue(value, name) {
  if (value === undefined || value === null) return {};
  return object(value, name);
}

function imageModelCandidates(ctx) {
  const req = (ctx && ctx.requestBody) || {};
  return [ctx && ctx.upstreamModel, ctx && ctx.model, req.model].map(trimmed).filter(Boolean);
}

function imageProfile(ctx) {
  for (const model of imageModelCandidates(ctx)) {
    if (Object.prototype.hasOwnProperty.call(IMAGE_MODELS, model)) return IMAGE_MODELS[model];
  }
  return DEFAULT_IMAGE_PROFILE;
}

function imageTask(ctx) {
  return imageModelCandidates(ctx).some((model) => Object.prototype.hasOwnProperty.call(IMAGE_MODELS, model)) || IMAGE_ACTIONS.includes(ctx && ctx.action);
}

function imageSizePixels(value) {
  const match = /^(\d+)\s*[xX×*]\s*(\d+)$/.exec(trimmed(value));
  if (!match) return null;
  const width = Number(match[1]);
  const height = Number(match[2]);
  if (!Number.isSafeInteger(width) || !Number.isSafeInteger(height) || width <= 0 || height <= 0) return null;
  return { width: width, height: height, pixels: width * height };
}

function imageSize(size, profile, layered) {
  if (size === undefined) return { size: undefined, pixels: layered ? null : IMAGE_PRESET_PIXELS["2K"] };
  if (typeof size !== "string") throw new Error("size must be a string");
  const preset = trimmed(size).toUpperCase();
  if (preset === "AUTO") {
    if (!layered) throw new Error("size auto is only supported with layer decomposition");
    return { size: "auto", pixels: null };
  }
  if (IMAGE_RESOLUTIONS.includes(preset)) {
    const allowed = layered ? LAYER_SIZE_PRESETS : profile.presets;
    if (!allowed.includes(preset)) throw new Error("size must be one of " + allowed.join(", ") + (layered ? ", auto" : " or width x height pixels"));
    return { size: preset, pixels: IMAGE_PRESET_PIXELS[preset] };
  }
  if (layered) throw new Error("layer decomposition sizes must be one of " + LAYER_SIZE_PRESETS.join(", ") + ", auto");
  const dims = imageSizePixels(size);
  if (!dims) throw new Error("size must be a resolution preset or width x height pixels");
  const ratio = dims.width / dims.height;
  if (dims.pixels < profile.minPixels || dims.pixels > profile.maxPixels || ratio < 1 / 16 || ratio > 16)
    throw new Error("size is outside the model's pixel and aspect-ratio limits");
  return { size: dims.width + "x" + dims.height, pixels: dims.pixels };
}

function isImageFilePlaceholder(value) {
  return (
    !!value &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    typeof value.__fileRef === "string" &&
    (value.encoding === "base64" || value.encoding === "dataUrl")
  );
}

function convertImage(ctx) {
  const req = ctx.requestBody || {};
  const model = trimmed(ctx.upstreamModel || req.model);
  if (!model) throw new Error("model is required");
  const profile = imageProfile(ctx);
  const body = {};
  for (const key of Object.keys(req)) {
    if (key === "model" || key === "stream" || req[key] === undefined) continue;
    body[key] = req[key];
  }
  body.model = model;
  if (body.n !== undefined && body.n !== null && body.n !== 1) throw new Error("n must be 1; use sequential_image_generation for multiple images");
  for (const key of ["watermark", "optimize_prompt", "layer_decomposition"]) {
    if (body[key] !== undefined && typeof body[key] !== "boolean") throw new Error(key + " must be a boolean");
  }
  const layered = body.layer_decomposition === true;
  if (layered && !profile.layers) throw new Error("layer_decomposition is not supported by this model");
  if (body.prompt !== undefined && typeof body.prompt !== "string") throw new Error("prompt must be a string");
  if (!layered && !trimmed(body.prompt)) throw new Error("prompt is required");
  const images = [];
  for (const key of ["image", "images"]) {
    if (body[key] === undefined) continue;
    images.push(...(Array.isArray(body[key]) ? body[key] : [body[key]]));
  }
  if (body.images !== undefined) {
    delete body.images;
    if (images.length === 1) body.image = images[0];
    else if (images.length > 1) body.image = images;
    else delete body.image;
  }
  for (const image of images) {
    if (image && typeof image === "object" && typeof image.__fileRef === "string" && image.encoding === "dataUrl") continue;
    const value = typeof image === "string" ? trimmed(image) : "";
    if (!isImageFilePlaceholder(image) && !/^https?:\/\//i.test(value) && !IMAGE_DATA_URI.test(value))
      throw new Error("image must be an HTTP URL or a data:image/<format>;base64 URL with a lowercase format");
  }
  if (layered && images.length !== 1) throw new Error("layer decomposition requires exactly one input image");
  if (images.length > profile.maxReferenceImages) throw new Error("at most " + profile.maxReferenceImages + " reference images are supported");
  if (body.response_format !== undefined && body.response_format !== "url") throw new Error("response_format must be url");
  if (body.seed !== undefined && !Number.isInteger(body.seed)) throw new Error("seed must be an integer");
  if (body.guidance_scale !== undefined && (typeof body.guidance_scale !== "number" || !Number.isFinite(body.guidance_scale)))
    throw new Error("guidance_scale must be a number");
  if (body.output_format !== undefined) {
    if (body.output_format !== "png" && body.output_format !== "jpeg") throw new Error("output_format must be png or jpeg");
    if (!profile.outputFormat) throw new Error("output_format is not supported by this model");
  }
  if (body.background !== undefined) {
    if (body.background !== "opaque" && body.background !== "transparent") throw new Error("background must be opaque or transparent");
    if (profile.background && body.background === "transparent" && images.length !== 1)
      throw new Error("background transparent requires exactly one input image");
    if (profile.background && body.background === "transparent" && body.output_format === "jpeg")
      throw new Error("background transparent cannot be combined with output_format jpeg");
  }
  if (body.tools !== undefined) {
    if (!Array.isArray(body.tools) || body.tools.some((tool) => !tool || typeof tool !== "object" || Array.isArray(tool) || !trimmed(tool.type)))
      throw new Error("tools must be an array of objects with a string type");
    if (!profile.tools) throw new Error("tools are not supported by this model");
  }
  const promptOptions = objectValue(body.optimize_prompt_options, "optimize_prompt_options");
  if (promptOptions.mode !== undefined) {
    if (promptOptions.mode !== "standard" && promptOptions.mode !== "fast") throw new Error("optimize_prompt_options.mode must be standard or fast");
    if (promptOptions.mode === "fast" && !profile.fastPromptMode) throw new Error("optimize_prompt_options.mode fast is not supported by this model");
  }
  const sequential = body.sequential_image_generation;
  if (sequential !== undefined && sequential !== "auto" && sequential !== "disabled") throw new Error("sequential_image_generation must be auto or disabled");
  if (sequential !== undefined && !profile.sequential) throw new Error("sequential_image_generation is not supported by this model");
  const options = objectValue(body.sequential_image_generation_options, "sequential_image_generation_options");
  let maxImages = 15;
  if (options.max_images !== undefined) {
    if (!Number.isInteger(options.max_images) || options.max_images < 1 || options.max_images > 15)
      throw new Error("max_images must be an integer between 1 and 15");
    maxImages = options.max_images;
  }
  const size = imageSize(body.size, profile, layered);
  if (size.size !== undefined) body.size = size.size;
  let imageCount = 1;
  if (layered) imageCount = MAX_IMAGE_OUTPUTS;
  else if (sequential === "auto") imageCount = Math.max(1, Math.min(maxImages, 15 - images.length));
  const higherTier = size.pixels === null || size.pixels > IMAGE_TIER_MAX_PIXELS;
  return {
    body: body,
    action: images.length ? "image_to_image" : "text_to_image",
    facts: seedreamUsageFacts(higherTier ? 0 : imageCount, higherTier ? imageCount : 0, images.length, layered),
  };
}

function imageEntries(body) {
  const data = body && body.data;
  if (Array.isArray(data)) return data.filter((item) => item && typeof item === "object" && !Array.isArray(item));
  if (data && typeof data === "object") return [data];
  return [];
}

function imageURLEntries(body) {
  return imageEntries(body).filter((item) => text(item.url));
}

function imagePayloads(body) {
  const entries = imageEntries(body);
  const urls = entries.filter((item) => text(item.url));
  const base64 = entries.filter((item) => text(item.b64_json));
  // 同一张图片被拆为 URL 和 Base64 两项时只计一次。
  const payloads = urls.length >= base64.length ? urls : base64;
  if (payloads.length > MAX_IMAGE_OUTPUTS) throw new Error("too many output images");
  const count = (body.usage || {}).generated_images;
  if (count !== undefined && (!Number.isInteger(count) || count < 0 || count > MAX_IMAGE_OUTPUTS || count !== payloads.length))
    throw new Error("invalid upstream generated_images");
  return payloads;
}

function imageUsage(body, profile) {
  const usage = body.usage || {};
  const payloads = imagePayloads(body);
  const facts = { image_count: payloads.length };
  if (hasLowerImageTier(profile)) {
    let lower = 0,
      higher = 0,
      sized = true;
    for (const item of payloads) {
      const dims = imageSizePixels(item.size);
      if (!dims) {
        sized = false;
        break;
      }
      if (dims.pixels > IMAGE_TIER_MAX_PIXELS) higher += 1;
      else lower += 1;
    }
    if (sized) {
      facts.images_up_to_1_5k = lower;
      facts.images_above_1_5k = higher;
    }
  } else {
    facts.images_above_1_5k = payloads.length;
  }
  if (Number.isInteger(usage.input_images) && usage.input_images >= 0 && usage.input_images <= MAX_REFERENCE_IMAGES) facts.input_images = usage.input_images;
  return facts;
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

function artifactData(ctx) {
  const data = (ctx && ctx.data) || {};
  if (data.data && typeof data.data === "object" && data.data.task_id && Object.prototype.hasOwnProperty.call(data.data, "data")) return data.data.data || {};
  return data;
}

function normalize(ctx, request, allowNumericStrings) {
  const req = object(request, "request body");
  const metadata = req.metadata === undefined ? {} : object(req.metadata, "metadata");
  const fields = Object.assign({}, metadata, req);
  const model = modelProfile(ctx, req.model);
  const limits = MODELS[model];
  const is25 = model === "doubao-seedance-2-5-260628";
  const maxDuration = is25 ? 30 : 15;
  if (allowNumericStrings) {
    for (const key of ["seconds", "duration", "seed", "priority", "execution_expires_after"]) {
      if (typeof fields[key] === "string" && fields[key].trim() !== "" && Number.isSafeInteger(Number(fields[key]))) fields[key] = Number(fields[key]);
    }
  }
  let duration = fields.seconds === undefined ? fields.duration : fields.seconds;
  if (fields.seconds !== undefined && fields.duration !== undefined && fields.seconds !== fields.duration) throw new Error("seconds and duration must match");
  if (duration !== undefined && duration !== -1) integer(duration, 4, maxDuration, "duration");
  const requestedResolution = fields.resolution === undefined ? fields.size : fields.resolution;
  if (requestedResolution !== undefined && !text(requestedResolution)) throw new Error("resolution must be a non-empty string");
  let resolution = text(requestedResolution).toLowerCase() || "720p";
  if (/^\d+[x*]\d+$/.test(resolution)) {
    const dimensions = resolution.split(/[x*]/).map(Number);
    if (dimensions.some((value) => !Number.isSafeInteger(value) || value <= 0)) throw new Error("size must contain positive pixel dimensions");
    const largest = Math.max(...dimensions);
    resolution = largest >= 3840 ? "4k" : largest >= 1920 ? "1080p" : largest >= 1280 ? "720p" : "480p";
  }
  if (!limits.includes(resolution)) throw new Error("Unsupported resolution for this Seedance model: " + resolution);
  for (const key of ["callback_url", "safety_identifier", "frames", "camera_fixed"]) {
    if (Object.prototype.hasOwnProperty.call(fields, key)) throw new Error(key + " is not supported by this video interface");
  }
  if (Object.prototype.hasOwnProperty.call(fields, "service_tier")) {
    if (!["default", "flex"].includes(fields.service_tier)) throw new Error("service_tier must be default or flex");
    if (!is25) throw new Error(model + " does not support service_tier");
  }
  if (Object.prototype.hasOwnProperty.call(fields, "draft")) {
    if (typeof fields.draft !== "boolean") throw new Error("draft must be a boolean");
    if (fields.draft) throw new Error("draft is not supported by this video interface");
  }
  if (fields.replyType !== undefined && fields.replyType !== "async") throw new Error("replyType must be async on the task API");
  if (fields.content !== undefined && !Array.isArray(fields.content)) throw new Error("content must be an array");
  if (fields.images !== undefined && !Array.isArray(fields.images)) throw new Error("images must be an array");
  const existingContent = fields.content || [];
  const content = [];
  for (const key of ["input_reference", "image"]) {
    if (fields[key] === undefined) continue;
    const url = referenceURL(fields[key]);
    if (key === "input_reference" && existingContent.some((item) => item && item.type === "image_url" && item.image_url && item.image_url.url === url))
      continue;
    content.push({ type: "image_url", image_url: { url } });
  }
  for (const url of fields.images || []) content.push({ type: "image_url", image_url: { url: referenceURL(url) } });
  for (const item of existingContent) content.push(item);
  const hasText = content.some((item) => item && item.type === "text" && text(item.text));
  const hasReference = content.some((item) => item && item.type !== "text");
  if (!hasText && (text(fields.prompt) || !hasReference)) content.push({ type: "text", text: fields.prompt || "" });
  if (!content.length || content.length > 128) throw new Error("content must contain 1 to 128 entries");
  let images = 0,
    audios = 0,
    videos = 0,
    firstFrames = 0,
    lastFrames = 0,
    unlabeledImages = 0,
    references = 0;
  for (const item of content) {
    object(item, "content entry");
    if (item.role !== undefined && item.role !== null && typeof item.role !== "string") throw new Error("role must be a string");
    const role = text(item.role);
    if (item.type === "text") {
      if (!text(item.text)) throw new Error("Text content must not be empty");
    } else if (["image_url", "video_url", "audio_url"].includes(item.type)) {
      referenceURL(object(item[item.type], item.type).url);
      if (item.type === "image_url") {
        images++;
        if (!role || role === "first_frame") {
          firstFrames++;
          if (!role) unlabeledImages++;
        } else if (role === "last_frame") lastFrames++;
        else if (role === "reference_image") references++;
        else throw new Error("image role is invalid");
      } else if (item.type === "video_url") {
        if (role !== "reference_video") throw new Error("video role must be reference_video");
        videos++;
        references++;
      } else {
        if (role !== "reference_audio") throw new Error("audio role must be reference_audio");
        audios++;
        references++;
      }
    } else throw new Error("Unsupported content type: " + String(item.type));
  }
  if (unlabeledImages > 0 && images !== 1) throw new Error("an image role is required when multiple images are provided");
  if (firstFrames > 1) throw new Error("content accepts at most one first_frame image");
  if (lastFrames > 1) throw new Error("content accepts at most one last_frame image");
  if (!is25 && lastFrames > 0 && firstFrames === 0) throw new Error("last_frame requires first_frame");
  if (firstFrames + lastFrames > 0 && references > 0) throw new Error("frame images cannot be mixed with reference media");
  if (!is25 && audios > 0 && images + videos === 0) throw new Error("audio input requires an image or video input");
  if (images > (is25 ? 30 : 9) || audios > (is25 ? 10 : 3)) throw new Error("Too many reference media entries");
  if (!is25 && videos > 3) throw new Error("content accepts at most 3 reference videos");
  const body = { model: text(ctx.model) || text(req.model), content, resolution };
  for (const key of OPTIONS) if (Object.prototype.hasOwnProperty.call(fields, key)) body[key] = fields[key];
  if (body.ratio !== undefined && !["16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"].includes(body.ratio)) throw new Error("Unsupported ratio");
  for (const key of ["generate_audio", "watermark", "return_last_frame"])
    if (body[key] !== undefined && typeof body[key] !== "boolean") throw new Error(key + " must be a boolean");
  if (body.output_format !== undefined && !["mp4", "mov"].includes(body.output_format)) throw new Error("output_format must be mp4 or mov");
  if (body.priority !== undefined) integer(body.priority, 0, 9, "priority");
  if (body.seed !== undefined) integer(body.seed, -1, 4294967295, "seed");
  if (body.execution_expires_after !== undefined) integer(body.execution_expires_after, 3600, 259200, "execution_expires_after");
  if (
    body.tools !== undefined &&
    (!Array.isArray(body.tools) || body.tools.some((tool) => !tool || typeof tool !== "object" || Array.isArray(tool) || tool.type !== "web_search"))
  )
    throw new Error("tools must be an array of web_search tools");
  const operation = body.omni_reference_task_type;
  if (operation !== undefined && (!is25 || !["auto", "reference", "edit", "extend"].includes(operation)))
    throw new Error("Unsupported omni_reference_task_type");
  if ((operation === "edit" || operation === "extend") && !videos) throw new Error("Video editing and extension require reference video");
  if (operation === "edit" && duration !== -1) throw new Error("Video editing requires duration -1");
  if (is25 && (firstFrames + lastFrames > 0 || operation === "edit" || operation === "extend")) {
    if (body.ratio !== undefined && body.ratio !== "adaptive") throw new Error("This task requires ratio adaptive");
    body.ratio = "adaptive";
  }
  // 智能时长是控制值；规范请求单独保存，不能让 -1 进入通用计费用量校验。
  if (duration === -1) body.automatic_duration = true;
  else if (duration !== undefined) body.duration = duration;
  return body;
}

function decode(ctx, req, allowNumericStrings) {
  const body = normalize(ctx, req, allowNumericStrings);
  return {
    kind: "submit",
    model: body.model,
    action: body.content.some((item) => item.type !== "text") ? "image_to_video" : "text_to_video",
    requestBody: body,
  };
}

function facts(ctx) {
  const req = object(ctx.requestBody, "request body");
  const model = modelProfile(ctx, req.model);
  const maxDuration = model === "doubao-seedance-2-5-260628" ? 30 : 15;
  const duration = req.automatic_duration || req.duration === undefined ? maxDuration : integer(req.duration, 4, maxDuration, "duration");
  const videoInput = req.content.some((item) => item.type === "video_url");
  // 提交仅作预算；参考视频按文档总时长上限预留，完成后使用媒体账单的 totalTokens。
  const seconds = duration + (videoInput ? maxDuration : 0);
  return {
    tokens: Math.ceil((seconds * PIXELS[req.resolution] * 24) / 1024),
    seconds: duration,
    resolution: req.resolution,
    video_input: videoInput ? "video" : "none",
  };
}

function auth(ctx) {
  return { Accept: "application/json", Authorization: "Bearer " + ctx.apiKey };
}

export function buildSubmitRequest(ctx) {
  if (imageTask(ctx)) {
    const converted = convertImage(ctx);
    // 图片使用 V3 原生接口，渠道地址中的既有 /api 前缀保持不变。
    const base = text(ctx.baseUrl).replace(/\/+$/, "").replace(/\/v1$/i, "");
    return {
      url: base + "/v3/images/generations",
      method: "POST",
      headers: Object.assign(auth(ctx), { "Content-Type": "application/json" }),
      body: converted.body,
      action: converted.action,
      rewriteModel: converted.body.model,
    };
  }
  const request = object(ctx.requestBody, "request body");
  const body = Object.assign({}, request, { model: ctx.upstreamModel || ctx.model, replyType: "async" });
  if (body.automatic_duration) body.duration = -1;
  delete body.automatic_duration;
  if (!text(ctx.publicTaskId)) throw new Error("Missing gateway task identity");
  return {
    url: apiRoot(ctx) + "/videos/generations",
    method: "POST",
    headers: Object.assign(auth(ctx), { "Content-Type": "application/json", "Idempotency-Key": ctx.publicTaskId }),
    body,
  };
}

export function parseSubmitResponse(ctx, response) {
  const body = object(response.body, "submission response");
  if (imageTask(ctx)) {
    if (!text(ctx.publicTaskId)) throw new Error("Missing gateway task identity");
    const entries = imageEntries(body);
    const urls = imageURLEntries(body);
    const failed = !urls.length && (body.error || entries.some((item) => item.error));
    if (!urls.length && !failed) throw new Error("Image generation returned no images");
    const fields = Object.assign({}, body);
    delete fields.data;
    const taskData = publicResult(ctx, fields);
    const images = entries.map((entry) => {
      const fields = Object.assign({}, entry);
      delete fields.url;
      const image = publicResult(ctx, fields);
      if (text(entry.url)) {
        // 下载签名属于图片地址，不能作为渠道凭证脱敏或替换模型名。
        const url = mediaURL(entry.url);
        if (text(ctx.apiKey) && (url.includes(ctx.apiKey) || url.includes(encodeURIComponent(ctx.apiKey))))
          throw new Error("Image URL contains invalid credentials");
        image.url = url;
      }
      return image;
    });
    taskData.data = Array.isArray(body.data) ? images : images[0];
    taskData.model = ctx.model;
    if (failed)
      return {
        taskId: ctx.publicTaskId,
        taskData,
        immediate: { status: "FAILURE", progress: "100%", reason: "Image generation failed" },
      };
    return {
      taskId: ctx.publicTaskId,
      taskData,
      immediate: { status: "SUCCESS", progress: "100%", url: text(imageURLEntries(taskData)[0].url) },
    };
  }
  if (!text(body.id)) throw new Error("Video service returned no task ID");
  const request = ctx.requestBody;
  const parameters = { resolution: request.resolution };
  if (request.automatic_duration) parameters.duration = -1;
  else if (request.duration !== undefined) parameters.duration = request.duration;
  for (const key of OPTIONS) {
    if (key !== "tools" && Object.prototype.hasOwnProperty.call(request, key)) parameters[key] = request[key];
  }
  const state = taskFields(
    Object.assign({}, ctx, { taskId: body.id }),
    { facts: facts(ctx), phase: "result", created_at: utils.unixNow(), request_parameters: parameters },
    body
  );
  // 请求秒数只作预留；完成用量不能继承这项估算。
  delete state.facts.seconds;
  return { taskId: body.id, taskData: taskSnapshot(ctx, "queued", state), state };
}

export function extractUsage(ctx) {
  if (imageTask(ctx)) {
    const usage = convertImage(ctx).facts;
    // 旧按次价格只乘输出数量；分档表达式读取完整图片用量。
    if (ctx.usagePurpose === "billing_ratios") return { image_count: usage.images_up_to_1_5k + usage.images_above_1_5k };
    return usage;
  }
  // 旧按次价格仍按次处理，避免把 token 预算乘入按次费用。
  return ctx.usagePurpose === "billing_ratios" ? null : facts(ctx);
}

export function extractUsageOnSubmit(ctx, body) {
  if (!imageTask(ctx)) return null;
  const count = imagePayloads(body || {}).length;
  return count ? { image_count: count } : {};
}

export function buildQueryRequest(ctx) {
  const phase = (ctx.state || {}).phase;
  const path = phase === "usage" ? "/mediaTaskInfo?taskId=" + encodeURIComponent(ctx.taskId) : "/videos/" + encodeURIComponent(ctx.taskId) + "/result";
  // 不跟随响应中的 result_url，查询始终使用管理员配置的同一服务地址。
  return { url: apiRoot(ctx) + path, method: "GET", headers: auth(ctx) };
}

export function parseTaskResult(ctx, raw) {
  const body = object(raw, "task response");
  const previousState = object(ctx.state, "persisted task state");
  if (previousState.phase === "usage") {
    if (body.code !== 0) throw new Error("Video usage query failed");
    const data = object(body.data, "media task data");
    if (["pending", "running", "processing"].includes(data.status))
      return { status: "IN_PROGRESS", progress: "95%", taskData: taskSnapshot(ctx, "running", previousState) };
    if (data.status !== "success") throw new Error("Video usage has an unrecognized or conflicting status");
    const usage = object(data.usage, "media task usage");
    const tokens = integer(usage.totalTokens, 0, Number.MAX_SAFE_INTEGER, "totalTokens");
    const reportedUsage = Object.assign({}, usage, { total_tokens: tokens });
    const state = taskFields(ctx, previousState, { usage: reportedUsage });
    // 保留原视频响应作为最终快照；用量就绪后再读一次结果，不改宿主的数据持久化约定。
    return {
      status: "IN_PROGRESS",
      progress: "99%",
      taskData: taskSnapshot(ctx, "running", state),
      state: Object.assign({}, state, { phase: "final", facts: Object.assign({}, state.facts, { tokens }) }),
    };
  }
  const state = taskFields(ctx, previousState, body);
  if (body.status === "failed") {
    if (state.phase === "final") throw new Error("Video result conflicts with its completed usage record");
    const error = body.error && typeof body.error === "object" && !Array.isArray(body.error) ? body.error : {};
    let message = text(error.message);
    if (text(ctx.apiKey)) message = message.split(ctx.apiKey).join("[redacted]");
    message = message.replace(/https?:\/\/\S+/gi, "[redacted URL]").replace(/\b(?:bearer\s+|sk-)[^\s,;]+/gi, "[redacted]");
    message = message.replace(/\b(?:api[_-]?key|token|secret|authorization)\s*[:=]\s*[^\s,;]+/gi, "[redacted]");
    message = message
      .split("")
      .map((character) => {
        const code = character.charCodeAt(0);
        return code < 32 || code === 127 ? " " : character;
      })
      .join("")
      .slice(0, 512);
    const diagnostic = {
      stage: ["generation", "processing"].includes(body.failure_stage) ? body.failure_stage : "",
      type: diagnosticIdentifier(error.type, 64, ctx.apiKey),
      request_id: diagnosticIdentifier(body["request-id"], 128, ctx.apiKey),
      message,
    };
    return {
      status: "FAILURE",
      progress: "100%",
      reason: "Video generation failed",
      taskData: taskSnapshot(ctx, "failed", state),
      state: Object.assign({}, state, { diagnostic }),
    };
  }
  if (body.status === "queued" || body.status === "pending") return { status: "QUEUED", progress: "10%", taskData: taskSnapshot(ctx, "queued", state), state };
  if (body.status === "running" || body.status === "processing")
    return { status: "IN_PROGRESS", progress: "50%", taskData: taskSnapshot(ctx, "running", state), state };
  if (body.status !== "succeeded") return { status: "UNKNOWN", reason: "Video task status is unavailable" };
  mediaURL(body.download_url);
  const measuredFacts = completionFacts(state, body);
  if (state.phase !== "final")
    return {
      status: "IN_PROGRESS",
      progress: "95%",
      taskData: taskSnapshot(ctx, "running", state),
      state: Object.assign({}, state, { phase: "usage", facts: measuredFacts }),
    };
  const completed = Object.assign({}, state, { facts: measuredFacts, media: { video: body.download_url, last_frame: text(body.last_frame_url) } });
  return {
    status: "SUCCESS",
    progress: "100%",
    url: mediaURL(completed.media.video),
    totalTokens: state.facts.tokens,
    taskData: taskSnapshot(ctx, "succeeded", completed),
    state: completed,
  };
}

function publicResult(ctx, value) {
  if (Array.isArray(value)) return value.map((item) => publicResult(ctx, item));
  if (typeof value === "string") {
    if (text(ctx.apiKey)) value = value.split(ctx.apiKey).join("[redacted]");
    if (text(ctx.taskId)) value = value.split(ctx.taskId).join(ctx.publicTaskId);
    if (text(ctx.upstreamModel) && ctx.upstreamModel !== ctx.model) value = value.split(ctx.upstreamModel).join(ctx.model);
    const origin = text(ctx.baseUrl).match(/^https?:\/\/[^/]+/i);
    if (origin) value = value.split(origin[0]).join("[redacted URL]");
    return value
      .replace(/anyllm(?:\.[a-z]+)?/gi, "[redacted]")
      .replace(/\b(?:bearer\s+|sk-)[^\s,;"<>]+/gi, "[redacted]")
      .replace(/\b(?:api[_-]?key|token|secret|authorization)\s*[:=]\s*[^\s,;"<>]+/gi, "[redacted]");
  }
  if (!value || typeof value !== "object") return value;
  const output = {};
  for (const key of Object.keys(value)) {
    const name = key.replace(/[^a-z0-9]/gi, "").toLowerCase();
    // 仅清理凭证、诊断与私有传输字段，业务扩展字段按原结构保留。
    if (
      /^(?:internal|upstream|provider|anyllm)/.test(name) ||
      [
        "proto",
        "prototype",
        "constructor",
        "apikey",
        "accesstoken",
        "refreshtoken",
        "token",
        "secret",
        "password",
        "authorization",
        "auth",
        "credentials",
        "headers",
        "cookie",
        "setcookie",
        "diagnostic",
        "diagnostics",
        "error",
        "debug",
        "stack",
        "failurestage",
        "requestid",
        "traceid",
        "resulturl",
        "downloadurl",
        "videourl",
        "lastframeurl",
      ].includes(name)
    )
      continue;
    output[key] = publicResult(ctx, value[key]);
  }
  return output;
}

function mergeTaskFields(previous, incoming) {
  const output = Object.assign({}, previous);
  for (const key of Object.keys(incoming)) {
    const value = incoming[key];
    const prior = output[key];
    output[key] =
      value && typeof value === "object" && !Array.isArray(value) && prior && typeof prior === "object" && !Array.isArray(prior)
        ? mergeTaskFields(prior, value)
        : value;
  }
  return output;
}

function taskFields(ctx, state, body) {
  const fields = publicResult(ctx, body);
  const taskId = encodeURIComponent(ctx.publicTaskId);
  const urls = {
    result_url: "/doubao/api/v3/contents/generations/tasks/" + taskId,
    download_url: body.download_url,
    last_frame_url: body.last_frame_url,
  };
  for (const key of Object.keys(urls)) {
    if (body[key] === null || body[key] === "") fields[key] = body[key];
    else if (text(body[key])) {
      if (key !== "result_url") {
        mediaURL(urls[key]);
        if (text(ctx.apiKey) && urls[key].includes(ctx.apiKey)) throw new Error("Video media URL contains channel credentials");
      }
      fields[key] = urls[key];
    }
  }
  return Object.assign({}, state, { task_fields: mergeTaskFields(state.task_fields || {}, fields) });
}

function taskSnapshot(ctx, status, state) {
  const body = Object.assign({}, state.task_fields, { id: ctx.publicTaskId, model: ctx.model, status });
  if (body.created_at === undefined) body.created_at = state.created_at;
  if (body.updated_at === undefined) body.updated_at = utils.unixNow();
  if (state.request_parameters) body.request_parameters = state.request_parameters;
  if (status === "succeeded") {
    body.content = Object.assign({}, body.content, { video_url: mediaURL(state.media.video) });
    if (state.media && text(state.media.last_frame))
      body.content.last_frame_url = mediaURL(state.media.last_frame);
    body.usage = Object.assign({}, body.usage, { total_tokens: state.facts.tokens });
    delete body.usage.totalTokens;
    if (state.facts.seconds !== undefined) body.duration = state.facts.seconds;
  }
  if (status === "failed") body.error = { code: "video_generation_failed", message: "Video generation failed" };
  return body;
}

function completionFacts(state, body) {
  const usageFacts = Object.assign({}, state.facts);
  if (body.duration !== undefined) {
    delete usageFacts.seconds;
    if (typeof body.duration === "number" && Number.isFinite(body.duration) && body.duration > 0 && body.duration <= 3600) usageFacts.seconds = body.duration;
  }
  const resolution = text(body.resolution || (body.content && body.content.resolution)).toLowerCase();
  if (Object.prototype.hasOwnProperty.call(PIXELS, resolution)) usageFacts.resolution = resolution;
  return usageFacts;
}

export function extractUsageOnComplete(task, result, body) {
  if (imageTask(task)) return imageUsage(body || {}, imageProfile(task));
  return result.status === "SUCCESS" && task.state && task.state.phase === "final" ? completionFacts(task.state, object(body, "task response")) : null;
}

export function publicErrorMessage(error) {
  if (error.local) return error.message;
  if (error.httpStatus === 429) return "Generation service is busy; retry later";
  if (error.httpStatus === 400) return "Generation request rejected; check the model, media, and parameters";
  if (error.httpStatus === 404) return "Generation task is unavailable";
  return "Generation request failed";
}

export function listArtifacts(task) {
  if (imageTask(task)) {
    if (task.status !== "SUCCESS") return [];
    return imageURLEntries(artifactData(task)).map((item, index) => {
      const artifact = { key: "image-" + (index + 1), type: "image" };
      if (item.output_format === "png") artifact.mimeType = "image/png";
      else if (item.output_format === "jpeg") artifact.mimeType = "image/jpeg";
      return artifact;
    });
  }
  if (task.status !== "SUCCESS" || !task.state || !task.state.media || !text(task.state.media.video)) return [];
  const items = [{ key: "video", type: "video" }];
  if (text(task.state.media.last_frame)) items.push({ key: "last-frame", type: "image" });
  return items;
}

export function buildContentRequest(ctx) {
  if (imageTask(ctx)) {
    const entries = imageURLEntries(artifactData(ctx));
    const index = entries.findIndex((_, position) => ctx.artifactKey === "image-" + (position + 1));
    if (index < 0) throw new Error("artifact_not_found");
    return { url: mediaURL(entries[index].url), method: "GET", credentialless: true };
  }
  const media = object(ctx.state && ctx.state.media, "task media");
  const url = ctx.artifactKey === "video" ? media.video : ctx.artifactKey === "last-frame" ? media.last_frame : "";
  if (!text(url)) throw new Error("artifact_not_found");
  // 产物地址按 GET 签名；客户端 HEAD 由宿主只返回响应头并关闭上游流。
  return { url: mediaURL(url), method: "GET", credentialless: true };
}

function videoStatus(_ctx, task) {
  const statuses = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "running", SUCCESS: "succeeded", FAILURE: "failed" };
  const body = {
    id: task.task_id,
    object: "video.generation",
    created: task.created_at,
    status: statuses[task.status] || "unknown",
    result_url: "/v1/videos/" + encodeURIComponent(task.task_id),
    download_url: null,
    error: null,
  };
  if (task.status === "SUCCESS") {
    const content = task.data && task.data.content;
    if (!content || !text(content.video_url)) throw new Error("Video artifact is unavailable");
    body.download_url = content.video_url;
  }
  if (task.status === "FAILURE") body.error = { code: "video_generation_failed", message: task.fail_reason || "Video generation failed" };
  return body;
}

// 与普通 Doubao Video 一致，以本地任务状态覆盖上游快照中的状态。
function nativeTask(task) {
  const data = task.data && typeof task.data === "object" && !Array.isArray(task.data) ? task.data : {};
  const output = Object.assign({}, data, { id: task.task_id });
  const statuses = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "running", SUCCESS: "succeeded", FAILURE: "failed" };
  output.status = statuses[task.status] || "queued";
  if (task.fail_reason === "task cancelled by user") output.status = "cancelled";
  else if (task.status === "FAILURE" && data.status === "expired") output.status = "expired";
  if (!output.created_at && task.created_at) output.created_at = task.created_at;
  if (!output.updated_at && task.updated_at) output.updated_at = task.updated_at;
  if (task.fail_reason && output.status === "failed" && !output.error) output.error = { message: task.fail_reason };
  return output;
}

export const native = {
  createTask(ctx) {
    if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
    const body = Object.assign({}, object(ctx.body.value, "request body"));
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
    ]) {
      if (body[key] === null) delete body[key];
    }
    return decode(ctx, body);
  },
  createImage: function (ctx) {
    if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
    const body = ctx.body.value;
    if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
    const model = trimmed(body.model);
    if (!model) throw new Error("model is required");
    if (body.stream !== undefined && body.stream !== false)
      throw new Error("stream is not supported on this route; the complete JSON response is returned once all images are generated");
    const requestBody = Object.assign({}, body, { model: model });
    const converted = convertImage({ model: model, requestBody: requestBody });
    return { kind: "submit", model: model, action: converted.action, requestBody: requestBody };
  },
  imageCreated: function (ctx, task) {
    return task.data && typeof task.data === "object" && !Array.isArray(task.data) ? task.data : {};
  },
  taskCreated(_ctx, task) {
    return { id: task.task_id };
  },
  taskStatus(_ctx, task) {
    return nativeTask(task);
  },
  listTasks(ctx) {
    if (!ctx.body || ctx.body.kind !== "none") throw new Error("request body is not allowed");
    const query = ctx.query || {};
    const allowed = ["page_num", "page_size", "filter.model", "filter.status", "filter.service_tier", "filter.task_ids"];
    for (const key of Object.keys(query)) {
      if (!allowed.includes(key)) throw new Error("unsupported query parameter: " + key);
      if (key !== "filter.task_ids" && query[key].length !== 1) throw new Error(key + " must be provided once");
    }
    const model = text((query["filter.model"] || [""])[0]);
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
    if (taskIds.length > 100 || taskIds.some((id) => !text(id) || id.length > 191)) throw new Error("filter.task_ids is invalid");
    return {
      kind: "query",
      model,
      taskIds: taskIds.map(text),
      actions: ["text_to_video", "image_to_video"],
      listOptions: { pageNum: pagination.page_num, pageSize: pagination.page_size, serviceTier, lookbackSeconds: 604800 },
    };
  },
  taskList(_ctx, result) {
    return { items: result.items.map(nativeTask), total: result.total };
  },
  error(ctx, error) {
    if (ctx.method === "GET" && ctx.params && ctx.params.task_id && error.httpStatus === 404 && error.code === "task_not_found")
      return { code: "task_not_exist", message: "task_not_exist", data: null };
    return { error: { code: error.code, message: error.httpStatus >= 500 ? "Generation request failed" : error.message } };
  },
};

function decodeVideo(ctx) {
  if (!ctx.body || !["json", "multipart"].includes(ctx.body.kind)) throw new Error("JSON or multipart body required");
  if (ctx.body.kind === "json") return decode(ctx, Object.assign({}, object(ctx.body.value, "request body"), { model: ctx.model }), true);
  if ((ctx.body.files || []).length) throw new Error("Reference media must use URLs");
  const req = {};
  for (const name of Object.keys(ctx.body.fields || {})) {
    const values = ctx.body.fields[name];
    if (values.length !== 1) throw new Error(name + " must be provided once");
    req[name] = values[0];
  }
  for (const name of ["generate_audio", "return_last_frame", "watermark", "camera_fixed", "draft"]) {
    if (req[name] === "true") req[name] = true;
    else if (req[name] === "false") req[name] = false;
  }
  for (const name of ["duration", "seconds", "frames", "seed", "priority", "execution_expires_after"]) {
    if (req[name] !== undefined) {
      if (!/^-?\d+$/.test(req[name])) throw new Error(name + " must be an integer");
      req[name] = Number(req[name]);
    }
  }
  for (const name of ["metadata", "content", "tools"]) {
    if (req[name] !== undefined) {
      try {
        req[name] = JSON.parse(req[name]);
      } catch {
        throw new Error(name + " must be valid JSON");
      }
    }
  }
  return decode(ctx, Object.assign(req, { model: ctx.model }), true);
}

function decodeResponses(ctx) {
  if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
  const req = object(ctx.body.value, "request body");
  if (req.input !== undefined && typeof req.input !== "string" && !Array.isArray(req.input)) throw new Error("input must be a string or array");
  if (req.images !== undefined && !Array.isArray(req.images)) throw new Error("images must be an array");
  const model = text(ctx.model);
  const input = responsesInput(req);
  if (imageTask({ upstreamModel: ctx.upstreamModel, model: model })) {
    const prompt = input.prompt || trimmed(req.prompt);
    const listed = Array.isArray(req.image) ? req.image : req.image === undefined ? [] : [req.image];
    const images = [];
    for (const image of listed.concat(req.images || [], input.images)) {
      if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
    }
    if (!prompt && !images.length) throw new Error("input is required");
    const requestBody = { model: model };
    if (prompt) requestBody.prompt = prompt;
    if (images.length) requestBody.image = images;
    for (const key of IMAGE_REQUEST_KEYS) {
      if (Object.prototype.hasOwnProperty.call(req, key)) requestBody[key] = req[key];
    }
    if (Array.isArray(req.tools)) {
      const tools = req.tools.filter((tool) => tool && typeof tool === "object" && !Array.isArray(tool) && tool.type === "web_search");
      if (tools.length) requestBody.tools = tools.map(() => ({ type: "web_search" }));
    }
    const converted = convertImage({ upstreamModel: ctx.upstreamModel, model: model, requestBody: requestBody });
    return { kind: "submit", model: model, action: converted.action, requestBody: requestBody };
  }
  const texts = [];
  const images = [];
  if (typeof req.input === "string") texts.push(req.input);
  else if (Array.isArray(req.input)) {
    for (const item of req.input) {
      if (typeof item === "string") {
        texts.push(item);
        continue;
      }
      if (!item || typeof item !== "object" || Array.isArray(item)) throw new Error("Response messages must be objects or strings");
      const parts = item.content === undefined ? [item] : Array.isArray(item.content) ? item.content : [item.content];
      for (const part of parts) {
        if (typeof part === "string") texts.push(part);
        else if (part && ["input_text", "output_text", "text"].includes(part.type) && typeof part.text === "string") texts.push(part.text);
        else if (part && ["input_image", "image_url"].includes(part.type)) {
          const url = part.image_url && typeof part.image_url === "object" ? part.image_url.url : part.image_url;
          images.push(referenceURL(url));
        } else throw new Error("Unsupported Responses content type");
      }
    }
  } else if (req.input !== undefined) throw new Error("input must be a string or array");
  const references = [];
  for (const url of [req.image, req.input_reference].concat(req.images || [], images)) {
    if (url === undefined || url === null) continue;
    const reference = referenceURL(url);
    if (!references.includes(reference)) references.push(reference);
  }
  const request = Object.assign({}, req, { model: ctx.model, prompt: texts.filter(text).join("\n") || text(req.prompt) });
  delete request.image;
  delete request.input_reference;
  delete request.images;
  if (references.length) request.images = references;
  return decode(ctx, request, true);
}

function outputText(ctx, task) {
  const data = artifactData(task);
  if (imageTask(ctx) || imageTask(task) || Array.isArray(data.data)) {
    const parts = imageURLEntries(data).map((_, index) => {
      const artifact = (ctx.artifacts || {})["image-" + (index + 1)];
      if (!artifact || !text(artifact.url)) throw new Error("Image artifact is unavailable");
      return "![Image " + (index + 1) + "](<" + artifact.url + ">)";
    });
    if (!parts.length) throw new Error("Image artifact is unavailable");
    return parts.join("\n\n");
  }
  const artifact = ctx.artifacts && ctx.artifacts.video;
  if (!artifact || !text(artifact.url)) throw new Error("Video artifact is unavailable");
  const url = artifact.url.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return '<video controls src="' + url + '"></video>';
}

export const protocols = {
  openai_image: {
    decodeRequest: function (ctx) {
      const model = trimmed(ctx.model);
      if (!model) throw new Error("model is required");
      const req = {};
      const images = [];
      if (ctx.body && ctx.body.kind === "json") {
        const value = ctx.body.value;
        if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("request body must be an object");
        Object.assign(req, value);
      } else if (ctx.body && ctx.body.kind === "multipart") {
        const fields = ctx.body.fields || {};
        for (const name of Object.keys(fields)) {
          const values = fields[name];
          if (!Array.isArray(values)) throw new Error(name + " must be a form field");
          if (/^images?(?:\[\d*\])?$/.test(name)) {
            images.push(...values);
            continue;
          }
          if (values.length > 1) throw new Error(name + " must be provided once");
          if (values.length > 0) req[name] = values[0];
        }
        for (const file of ctx.body.files || []) {
          if (file.field === "mask") throw new Error("mask is not supported");
          if (!/^image(?:\[\d*\])?$/.test(file.field)) {
            throw new Error("unsupported image upload field: " + file.field);
          }
          const mimeType = trimmed(file.mimeType).toLowerCase();
          images.push({
            __fileRef: file.ref,
            encoding: "dataUrl",
            mimeType: /^image\/[a-z0-9.+-]+$/.test(mimeType) ? mimeType : "image/png",
            maxBytes: MAX_INPUT_IMAGE_BYTES,
          });
        }
        for (const key of ["n", "seed", "guidance_scale"]) {
          if (req[key] === undefined) continue;
          const value = Number(req[key]);
          if (!Number.isFinite(value)) throw new Error(key + " must be a number");
          req[key] = value;
        }
        for (const key of ["stream", "watermark", "optimize_prompt", "layer_decomposition"]) {
          if (req[key] === undefined) continue;
          if (req[key] !== "true" && req[key] !== "false") throw new Error(key + " must be true or false");
          req[key] = req[key] === "true";
        }
        for (const key of ["tools", "optimize_prompt_options", "sequential_image_generation_options"]) {
          if (req[key] === undefined) continue;
          try {
            req[key] = JSON.parse(req[key]);
          } catch (error) {
            throw new Error(key + " must be a JSON value", { cause: error });
          }
        }
      } else {
        throw new Error("JSON or multipart body required");
      }

      if (req.stream !== undefined && req.stream !== false) {
        throw new Error("stream is not supported; the complete image response is returned once all images are generated");
      }
      const responseFormat = req.response_format;
      if (responseFormat !== undefined && responseFormat !== "url" && responseFormat !== "b64_json") {
        throw new Error("response_format must be url or b64_json");
      }
      if (req.n !== undefined && req.n !== null && req.n !== 1) throw new Error("n must be 1; use sequential_image_generation for multiple images");
      if (req.mask !== undefined) throw new Error("mask is not supported");
      if (images.length > 0) {
        const existing = req.image === undefined ? [] : Array.isArray(req.image) ? req.image : [req.image];
        req.image = existing.concat(images);
      }
      const requestBody = { model: model };
      for (const key of IMAGE_REQUEST_KEYS.concat(["prompt", "image", "images", "background", "tools"])) {
        if (key !== "response_format" && Object.prototype.hasOwnProperty.call(req, key)) requestBody[key] = req[key];
      }
      const converted = convertImage({
        model: model,
        upstreamModel: ctx.upstreamModel,
        requestBody: requestBody,
      });
      if (ctx.operation === "edit" && converted.action !== "image_to_image") throw new Error("image is required");
      return {
        kind: "submit",
        model: model,
        action: converted.action,
        requestBody: requestBody,
      };
    },
    render: function (ctx, task) {
      const body = artifactData(task);
      const data = imagePayloads(body).map(function (image) {
        const entry = {};
        if (trimmed(image.url)) entry.url = trimmed(image.url);
        else if (trimmed(image.b64_json)) entry.b64_json = trimmed(image.b64_json);
        if (typeof image.revised_prompt === "string") entry.revised_prompt = image.revised_prompt;
        return entry;
      });
      const response = { created: task.created_at, data: data };
      if (body.usage && typeof body.usage === "object" && !Array.isArray(body.usage)) {
        response.metadata = { usage: body.usage };
      }
      return response;
    },
  },
  openai_video: { decodeRequest: decodeVideo, render: videoStatus },
  openai_responses: {
    decodeRequest: decodeResponses,
    renderEvents(ctx, task, previous) {
      const status = String(task.status || "UNKNOWN").toUpperCase();
      const value = Number(String(task.progress || "").replace("%", ""));
      const progress = Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
      const state = { status, progress };
      if (task.status === "SUCCESS")
        return { events: previous && previous.status === "SUCCESS" ? [] : [{ type: "output", data: outputText(ctx, task) }], state, done: true };
      if (task.status === "FAILURE")
        return { events: [{ type: "error", code: "task_failed", message: task.fail_reason || "Video generation failed" }], state, done: true };
      const event = { type: "progress", message: status.toLowerCase() };
      if (progress !== null) event.progress = progress;
      return {
        events: previous && previous.status === status && previous.progress === progress ? [] : [event],
        state,
        done: false,
      };
    },
    renderFinal(ctx, task) {
      return {
        output: [
          {
            type: "message",
            role: "assistant",
            status: "completed",
            content: [{ type: "output_text", text: outputText(ctx, task), annotations: [], logprobs: [] }],
          },
        ],
        metadata: { vendor: "doubao" },
      };
    },
  },
};
