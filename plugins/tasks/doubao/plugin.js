// Fast、Mini 的官方输出范围独立于标准版，元数据和提交校验共用此声明。
const LOW_RESOLUTION_MODELS = ["doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615"];
const LOW_RESOLUTIONS = ["480p", "720p"];

export const meta = {
  apiVersion: 1,
  key: "doubao",
  name: "Doubao Video",
  icon: "Doubao.Color",
  description: {
    en: "Volcengine Doubao Seedance video generation (text-to-video, image-to-video, and video-to-video)",
    zh: "火山引擎豆包 Seedance 视频生成（文生视频、图生视频、视频生视频）",
  },
  version: "1.1.2",
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

// 统一原生、兼容接口及最终渠道映射后的分辨率校验，缺省时按模型可用上限预估。
function requestResolution(model, req) {
  const metadata = req.metadata || {};
  const raw = trimmed(metadata.resolution || req.resolution || req.size).toLowerCase();
  const restricted = LOW_RESOLUTION_MODELS.includes(trimmed(model).toLowerCase());
  const recognized = ["480p", "720p", "1080p", "4k"].includes(raw) || /^\d+[x*]\d+$/.test(raw);
  const resolution = recognized ? normalizeResolution(raw) : restricted ? "720p" : "1080p";
  if (restricted && raw && (!recognized || !LOW_RESOLUTIONS.includes(resolution))) {
    throw new Error(model + " only supports 480p and 720p resolution");
  }
  return resolution;
}

// metadata 与兼容接口顶层参数都可能携带计费数量，必须在构建请求和预扣前执行同一边界。
function validateGenerationParameters(req) {
  const metadata = req.metadata || {};
  const raw = req.seconds !== undefined ? req.seconds : req.duration !== undefined ? req.duration : metadata.duration;
  if (raw !== undefined) {
    const duration = typeof raw === "number" || (typeof raw === "string" && raw.trim() !== "") ? Number(raw) : NaN;
    if (!Number.isInteger(duration) || (duration !== -1 && (duration < 2 || duration > 30))) {
      throw new Error("duration must be -1 or an integer between 2 and 30");
    }
  }
  if (
    metadata.frames !== undefined &&
    (!Number.isInteger(metadata.frames) || metadata.frames < 29 || metadata.frames > 289 || (metadata.frames - 25) % 4 !== 0)
  ) {
    throw new Error("frames must be an integer between 29 and 289 matching 25 + 4n");
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
    requestResolution(model, { metadata: body });
    // 原生数值字段保持官方类型；智能时长 -1 保留，拒绝绕过预扣估算的异常数量。
    if (body.duration !== undefined && (!Number.isInteger(body.duration) || (body.duration !== -1 && (body.duration < 2 || body.duration > 30)))) {
      throw new Error("duration must be -1 or an integer between 2 and 30");
    }
    if (body.frames !== undefined && (!Number.isInteger(body.frames) || body.frames < 29 || body.frames > 289 || (body.frames - 25) % 4 !== 0)) {
      throw new Error("frames must be an integer between 29 and 289 matching 25 + 4n");
    }
    if (body.content !== undefined && !Array.isArray(body.content)) throw new Error("content must be an array");
    const content = Array.isArray(body.content) ? body.content : [];
    const texts = [];
    let hasReference = false;
    for (const item of content) {
      if (!item || typeof item !== "object" || Array.isArray(item)) continue;
      if (item.type === "text" && typeof item.text === "string") texts.push(item.text);
      else hasReference = true;
    }
    if (!texts.length && !hasReference) throw new Error("content is required");
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
    const intent = { kind: "submit", model: model, action: hasReference ? "image_to_video" : "text_to_video", requestBody: requestBody };
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
    // 方舟官方 DELETE 的成功响应固定为空 JSON 对象。
    return {};
  },
  error: function (ctx, error) {
    return { error: { code: error.code, message: error.message } };
  },
};

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody;
  validateGenerationParameters(req);
  const metadata = req.metadata || {};
  const body = Object.assign({ model: req.model || "", content: [] }, metadata);
  const imageContent = [];
  const images = Array.isArray(req.images) ? req.images : [];
  for (const url of images) imageContent.push({ type: "image_url", image_url: { url: url } });
  const metadataContent = Array.isArray(body.content) ? body.content : [];
  // 原生 content 的顺序、多个文本项和素材引用都必须原样保留。
  body.content = imageContent.concat(metadataContent);
  const hasReference = body.content.some((item) => item && item.type !== "text");
  if (!metadataContent.some((item) => item && item.type === "text") && (trimmed(req.prompt) || !hasReference))
    body.content.push({ type: "text", text: req.prompt || "" });
  if (Array.isArray(body.content)) body.content = rewriteDraftTaskContent(body.content, ctx.originTasks);
  // duration 别名和智能时长 -1 都必须真正发送；不能截断小数后丢失请求数量。
  const seconds = req.seconds !== undefined ? req.seconds : req.duration;
  if (seconds !== undefined) body.duration = Number(seconds);
  body.model = ctx.upstreamModel || body.model;
  // 渠道别名映射后再次校验；兼容接口的 size/resolution 必须与实际发往上游的值一致。
  requestResolution(body.model, req);
  if (!body.resolution && req.resolution) body.resolution = req.resolution;
  if (!body.resolution && req.size) body.resolution = normalizeResolution(req.size);
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
  const metadata = (ctx.requestBody && ctx.requestBody.metadata) || {};
  return { taskId: resp.body.id, taskData: Object.assign({ service_tier: metadata.service_tier || "default" }, resp.body) };
}

export function extractUsage(ctx) {
  const req = ctx.requestBody || {};
  validateGenerationParameters(req);
  const metadata = req.metadata || {};
  const resolution = requestResolution(ctx.upstreamModel || ctx.model || req.model, req);
  if (ctx.usagePurpose === "billing_ratios") {
    const ratio = videoInputRatio(ctx.upstreamModel || ctx.model, metadata.resolution || req.resolution || (req.size ? resolution : ""), metadata.content);
    return ratio === 1 ? null : { video_input_ratio: ratio };
  }
  // 官方 frames 优先于 duration；非整数秒不能向下取整后少预扣。
  const frames = Number(metadata.frames);
  let seconds = Number.isFinite(frames) && frames > 0 ? frames / 24 : Number(req.seconds || req.duration || metadata.duration || 0);
  if (!Number.isFinite(seconds) || seconds <= 0) {
    seconds = (ctx.upstreamModel || ctx.model || req.model) === "doubao-seedance-2-5-260628" ? 30 : 15;
  }
  if (seconds <= 0) seconds = 5;
  seconds = Math.min(seconds, 3600);
  return {
    tokens: estimateTokens(seconds, resolution),
    resolution: resolution,
    video_input: hasVideo(metadata.content) ? "video" : "none",
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
    !response.body ||
    typeof response.body !== "object" ||
    Array.isArray(response.body) ||
    Object.keys(response.body).length !== 0
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
  if (tokens === null || (tokens === 0 && totalTokens !== null)) tokens = totalTokens;
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
      if (!prompt && images.length === 0) throw new Error("input is required");
      const metadata = Object.assign({}, req.metadata || {});
      if (Object.prototype.hasOwnProperty.call(req, "resolution")) metadata.resolution = req.resolution;
      else if (req.size && !metadata.resolution) metadata.resolution = normalizeResolution(req.size);
      const requestBody = { model: model, prompt: prompt, metadata: metadata };
      requestResolution(model, requestBody);
      if (images.length) requestBody.images = images;
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.seconds = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.seconds = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      validateGenerationParameters(requestBody);
      const intent = { kind: "submit", model: model, action: images.length ? "image_to_video" : "text_to_video", requestBody: requestBody };
      const originTaskIds = draftTaskIds(metadata.content);
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
      const req = ctx.body.value;
      requestResolution(ctx.model, req);
      validateGenerationParameters(req);
      const seconds = req.seconds === undefined ? req.duration : req.seconds;
      if (seconds !== undefined && (!Number.isFinite(Number(seconds)) || Number(seconds) <= 0 || Number(seconds) > 3600))
        throw new Error("seconds must be between 1 and 3600");
      return {
        kind: "submit",
        model: ctx.model,
        action: req.input_reference || req.image ? "image_to_video" : "text_to_video",
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
    if ((ctx.body.files || []).length) throw new Error("Doubao requires image and video references to be URLs inside metadata.content");
    requestResolution(ctx.model, req);
    validateGenerationParameters(req);
    if (req.seconds !== undefined) req.seconds = Number(req.seconds);
    else if (req.duration !== undefined) req.seconds = Number(req.duration);
    const seconds = req.seconds === undefined ? req.duration : req.seconds;
    if (seconds !== undefined && (!Number.isFinite(Number(seconds)) || Number(seconds) <= 0 || Number(seconds) > 3600))
      throw new Error("seconds must be between 1 and 3600");
    return {
      kind: "submit",
      model: ctx.model,
      action: req.input_reference || req.image ? "image_to_video" : "text_to_video",
      requestBody: Object.assign({}, req, { model: ctx.model }),
    };
  },
  render: function (ctx, task) {
    return legacyRenderers.openai_video(task);
  },
};
