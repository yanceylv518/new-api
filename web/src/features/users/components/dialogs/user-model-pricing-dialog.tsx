/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import axios from 'axios'
import {
  BadgePercent,
  ChevronLeft,
  ChevronRight,
  Loader2,
  Search,
  X,
} from 'lucide-react'
import { useDeferredValue, useEffect, useMemo, useState } from 'react'
import { useForm, type FieldErrors } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { handleServerError } from '@/lib/handle-server-error'

import { getUserModelPricing, replaceUserModelPricing } from '../../api'
import {
  buildUserModelPricingPayload,
  buildUserModelPricingRows,
  createUserModelPricingFormSchema,
  normalizeUserModelPricingModelName,
  type UserModelPricingFormValues,
} from '../../lib/user-model-pricing-form'
import type { User } from '../../types'

interface UserModelPricingDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  user: Pick<User, 'id' | 'username'>
}

type DiscountFilter = 'all' | 'configured' | 'unconfigured'

// 固定每页模型数量，配合列表滚动避免大量模型撑开弹窗。
const MODEL_PAGE_SIZE = 25

/** 管理员编辑单个用户的完整模型折扣比例规则。 */
export function UserModelPricingDialog(props: UserModelPricingDialogProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [searchTerm, setSearchTerm] = useState('')
  const [currentPage, setCurrentPage] = useState(1)
  const [discountFilter, setDiscountFilter] = useState<DiscountFilter>('all')
  // 以规范化模型名保存选择，翻页和筛选不改变批量操作的目标。
  const [selectedModels, setSelectedModels] = useState<Set<string>>(new Set())
  const [batchDiscount, setBatchDiscount] = useState('100')
  const [batchError, setBatchError] = useState<string | null>(null)
  // 将筛选计算延后到输入空闲时，避免模型数量较大时阻塞搜索框输入。
  const deferredSearchTerm = useDeferredValue(searchTerm)
  const formSchema = useMemo(() => createUserModelPricingFormSchema(t), [t])
  const form = useForm<UserModelPricingFormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: { items: [] },
    // 分页和筛选会卸载不可见行，但提交时必须保留这些行的值。
    shouldUnregister: false,
  })
  const query = useQuery({
    queryKey: ['user-model-pricing', props.user.id],
    queryFn: () => getUserModelPricing(props.user.id),
    enabled: props.open,
    staleTime: 0,
  })

  const collection = query.data?.data
  // 编辑会话冻结规则、目录与 revision，后台查询更新不得重建行索引或覆盖未保存草稿。
  const [editorSnapshot, setEditorSnapshot] = useState<{
    userId: number
    collection: NonNullable<typeof collection>
    rows: ReturnType<typeof buildUserModelPricingRows>
  } | null>(null)
  // 仅在打开且初次查询完成时建立会话，条件更新在渲染提交前完成，避免 effect 连锁重置。
  if (!props.open && editorSnapshot) {
    setEditorSnapshot(null)
    setSelectedModels(new Set())
    setBatchDiscount('100')
    setBatchError(null)
  } else if (
    props.open &&
    editorSnapshot?.userId !== props.user.id &&
    collection &&
    query.isSuccess &&
    query.data.success &&
    !query.isFetching &&
    Array.isArray(collection.model_names)
  ) {
    // 选择和批量输入只属于当前编辑会话，切换用户或重新打开时全部重置。
    setSelectedModels(new Set())
    setBatchDiscount('100')
    setBatchError(null)
    setEditorSnapshot({
      userId: props.user.id,
      collection,
      // 目录与规则由同一管理接口返回，加载失败时一次重试即可恢复整个编辑会话。
      rows: buildUserModelPricingRows(
        collection.model_names.map((model_name) => ({ model_name }))
      ),
    })
  }
  const activeSnapshot =
    editorSnapshot?.userId === props.user.id ? editorSnapshot : null
  // 复用归一化后的折扣规则判断模型是否已经配置过专属折扣。
  const configuredDiscounts = useMemo(() => {
    const discounts = new Map<string, number>()
    for (const item of activeSnapshot?.collection.items ?? []) {
      discounts.set(
        normalizeUserModelPricingModelName(item.model_name),
        item.discount_bps / 100
      )
    }
    return discounts
  }, [activeSnapshot])

  const pricingRows = useMemo(
    () => activeSnapshot?.rows ?? [],
    [activeSnapshot]
  )

  useEffect(() => {
    if (!activeSnapshot) return
    const discounts = new Map(
      activeSnapshot.collection.items.map((item) => [
        normalizeUserModelPricingModelName(item.model_name),
        item.discount_bps / 100,
      ])
    )
    form.reset({
      // 所有启用模型都展示，未配置专属折扣时默认按原价计费。
      items: activeSnapshot.rows.map((model) => ({
        model_name: model.model_name,
        discount_percent:
          discounts.get(normalizeUserModelPricingModelName(model.model_name)) ??
          100,
      })),
    })
  }, [activeSnapshot, form])

  const handleOpenChange = (open: boolean) => {
    if (!open) {
      // 关闭时清空筛选状态，下一次打开默认从第一页展示全部模型。
      setSearchTerm('')
      setCurrentPage(1)
      setDiscountFilter('all')
    }
    props.onOpenChange(open)
  }

  const mutation = useMutation({
    mutationFn: (values: UserModelPricingFormValues) =>
      replaceUserModelPricing(props.user.id, {
        ...buildUserModelPricingPayload(
          values,
          activeSnapshot?.collection.items
        ),
        revision: activeSnapshot?.collection.revision ?? 0,
      }),
    onSuccess: async (response) => {
      if (!response.success) {
        toast.error(response.message || t('Failed to save model pricing'))
        return
      }
      await queryClient.invalidateQueries({ queryKey: ['pricing'] })
      await queryClient.invalidateQueries({
        queryKey: ['user-model-pricing', props.user.id],
      })
      toast.success(t('Model pricing saved'))
      handleOpenChange(false)
    },
    onError: async (error) => {
      if (axios.isAxiosError(error) && error.response?.status === 409) {
        await queryClient.invalidateQueries({
          queryKey: ['user-model-pricing', props.user.id],
        })
        toast.error(
          t(
            'Model pricing changed by another administrator. Your edits were kept. Reopen the dialog to load the latest rules.'
          )
        )
        return
      }
      handleServerError(error)
    },
  })

  const isLoading = !activeSnapshot && (query.isLoading || query.isFetching)
  const hasLoadError =
    !activeSnapshot &&
    (query.isError ||
      (query.data !== undefined &&
        (!query.data.success || !Array.isArray(collection?.model_names))))

  // 预先缓存小写模型名，搜索时只扫描稳定的行索引，减少重复字符串处理和对象创建。
  const modelRows = useMemo(
    () =>
      pricingRows.map((model, index) => ({
        model,
        index,
        searchName: model.model_name.toLowerCase(),
        isConfigured: configuredDiscounts.has(
          normalizeUserModelPricingModelName(model.model_name)
        ),
      })),
    [configuredDiscounts, pricingRows]
  )
  // 只排序显示索引副本，保留表单字段的原始索引，避免输入时焦点和值发生错位。
  // 排序依据是已保存的折扣规则，编辑中的空值不会触发行位置跳动。
  const orderedRows = useMemo(
    () =>
      [...modelRows].sort(
        (left, right) =>
          Number(right.isConfigured) - Number(left.isConfigured) ||
          left.index - right.index
      ),
    [modelRows]
  )
  // 校验失败时按表单原始索引恢复筛选和分页，让被卸载的错误行重新挂载。
  const handleInvalid = (errors: FieldErrors<UserModelPricingFormValues>) => {
    const invalidItemKey = Object.keys(errors.items ?? {}).find((key) =>
      /^\d+$/.test(key)
    )
    if (invalidItemKey === undefined) return

    const invalidItemIndex = Number(invalidItemKey)
    const orderedIndex = orderedRows.findIndex(
      (row) => row.index === invalidItemIndex
    )
    if (orderedIndex < 0) return

    setSearchTerm('')
    setDiscountFilter('all')
    setCurrentPage(Math.floor(orderedIndex / MODEL_PAGE_SIZE) + 1)
  }
  const filteredRows = useMemo(() => {
    const keyword = deferredSearchTerm.trim().toLowerCase()
    return orderedRows.filter(({ searchName, isConfigured }) => {
      const matchesSearch = !keyword || searchName.includes(keyword)
      const matchesDiscountFilter =
        discountFilter === 'all' ||
        (discountFilter === 'configured' && isConfigured) ||
        (discountFilter === 'unconfigured' && !isConfigured)
      return matchesSearch && matchesDiscountFilter
    })
  }, [deferredSearchTerm, discountFilter, orderedRows])
  const totalModels = pricingRows.length
  const totalFilteredModels = filteredRows.length
  const totalPages = Math.max(
    1,
    Math.ceil(totalFilteredModels / MODEL_PAGE_SIZE)
  )
  const safeCurrentPage = Math.min(currentPage, totalPages)
  const paginatedRows = useMemo(() => {
    const startIndex = (safeCurrentPage - 1) * MODEL_PAGE_SIZE
    return filteredRows.slice(startIndex, startIndex + MODEL_PAGE_SIZE)
  }, [filteredRows, safeCurrentPage])
  const displayStart =
    totalFilteredModels === 0 ? 0 : (safeCurrentPage - 1) * MODEL_PAGE_SIZE + 1
  const displayEnd =
    totalFilteredModels === 0
      ? 0
      : Math.min(safeCurrentPage * MODEL_PAGE_SIZE, totalFilteredModels)
  const showPagination = totalFilteredModels > MODEL_PAGE_SIZE
  const selectedFilteredCount = filteredRows.reduce(
    (count, row) => count + Number(selectedModels.has(row.model.model_name)),
    0
  )
  const allFilteredSelected =
    totalFilteredModels > 0 && selectedFilteredCount === totalFilteredModels

  // 复用单行校验，并一次检查完整规则数量；应用只更新草稿，不发起保存请求。
  const applyBatchDiscount = () => {
    if (mutation.isPending || !activeSnapshot || selectedModels.size === 0) {
      return
    }
    const result =
      formSchema.shape.items.element.shape.discount_percent.safeParse(
        batchDiscount.trim() === '' ? undefined : Number(batchDiscount)
      )
    if (!result.success) {
      setBatchError(result.error.issues[0].message)
      return
    }
    const items = form
      .getValues('items')
      .map((item) =>
        selectedModels.has(item.model_name)
          ? { ...item, discount_percent: result.data }
          : item
      )
    if (
      buildUserModelPricingPayload({ items }, activeSnapshot.collection.items)
        .items.length > 1000
    ) {
      setBatchError(t('At most 1000 model pricing rules are allowed'))
      return
    }
    const changedFields: `items.${number}.discount_percent`[] = []
    items.forEach((item, index) => {
      if (!selectedModels.has(item.model_name)) return
      const name = `items.${index}.discount_percent` as const
      form.setValue(name, item.discount_percent, {
        shouldDirty: true,
        shouldTouch: true,
      })
      changedFields.push(name)
    })
    setBatchError(null)
    // 批量写完再统一更新字段错误，避免每写一行就对完整表单重复校验。
    void form.trigger(changedFields)
  }
  // 显式渲染筛选标签，避免下拉框回退显示内部枚举值（如 all）。
  let discountFilterLabel = t('Unconfigured discounts')
  if (discountFilter === 'all') {
    discountFilterLabel = t('All')
  } else if (discountFilter === 'configured') {
    discountFilterLabel = t('Configured discounts')
  }

  // 连续的 flex 高度约束将可用空间传给模型滚动区，同时保留少量模型时的内容自适应高度。
  return (
    <Dialog
      open={props.open}
      onOpenChange={handleOpenChange}
      title={
        <span className='flex items-center gap-2'>
          <BadgePercent aria-hidden='true' />
          {t('User model pricing')}
        </span>
      }
      description={t(
        'Set model-specific discount percentages for {{username}}',
        {
          username: props.user.username,
        }
      )}
      contentClassName='flex max-h-[calc(100dvh-2rem)] sm:max-h-[85vh] sm:max-w-3xl'
      contentHeight='auto'
      bodyClassName='flex min-h-0 flex-1 flex-col overflow-hidden'
      bodyWrapperClassName='flex flex-col overflow-y-hidden'
      footerClassName='flex-row justify-end [&>button]:flex-1 sm:[&>button]:flex-none'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => handleOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form='user-model-pricing-form'
            disabled={
              mutation.isPending || !activeSnapshot || isLoading || hasLoadError
            }
          >
            {mutation.isPending && <Loader2 className='animate-spin' />}
            {t('Save')}
          </Button>
        </>
      }
    >
      {isLoading && (
        <div className='flex h-48 items-center justify-center'>
          <Loader2 className='text-muted-foreground size-6 animate-spin' />
        </div>
      )}
      {!isLoading && hasLoadError && (
        <div className='flex h-48 flex-col items-center justify-center gap-3 text-center'>
          <p className='text-muted-foreground text-sm'>
            {t('Failed to load model pricing')}
          </p>
          <Button
            type='button'
            variant='outline'
            onClick={() => query.refetch()}
          >
            {t('Retry')}
          </Button>
        </div>
      )}
      {!isLoading && !hasLoadError && (
        <Form {...form}>
          <form
            id='user-model-pricing-form'
            noValidate
            onSubmit={form.handleSubmit(
              (values) => mutation.mutate(values),
              handleInvalid
            )}
            className='flex min-h-0 flex-1 flex-col gap-3 overflow-hidden'
          >
            <div className='flex shrink-0 flex-col gap-2'>
              <div className='flex flex-col gap-2 sm:flex-row sm:items-center'>
                <div className='relative min-w-0 flex-1'>
                  <Search
                    aria-hidden='true'
                    className='text-muted-foreground pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2'
                  />
                  <Input
                    value={searchTerm}
                    onChange={(event) => {
                      setSearchTerm(event.target.value)
                      setCurrentPage(1)
                    }}
                    placeholder={t('Search models...')}
                    className='pl-9'
                    aria-label={t('Search models')}
                  />
                </div>
                <Select
                  value={discountFilter}
                  onValueChange={(value) => {
                    if (
                      value !== 'all' &&
                      value !== 'configured' &&
                      value !== 'unconfigured'
                    ) {
                      return
                    }
                    setDiscountFilter(value)
                    setCurrentPage(1)
                  }}
                >
                  <SelectTrigger
                    className='w-full sm:w-44'
                    aria-label={t('Discount filter')}
                  >
                    <SelectValue>{discountFilterLabel}</SelectValue>
                  </SelectTrigger>
                  <SelectContent alignItemWithTrigger={false}>
                    <SelectGroup>
                      <SelectItem value='all'>{t('All')}</SelectItem>
                      <SelectItem value='configured'>
                        {t('Configured discounts')}
                      </SelectItem>
                      <SelectItem value='unconfigured'>
                        {t('Unconfigured discounts')}
                      </SelectItem>
                    </SelectGroup>
                  </SelectContent>
                </Select>
              </div>
              {totalModels > 0 && (
                <span className='text-muted-foreground shrink-0 text-xs'>
                  {t('Showing')} {displayStart}-{displayEnd} {t('of')}{' '}
                  {totalFilteredModels} {t('models')}
                </span>
              )}
            </div>

            {activeSnapshot && selectedModels.size > 0 && (
              <div
                className='flex shrink-0 flex-col gap-2'
                data-slot='model-pricing-batch'
              >
                <div className='flex flex-wrap items-center gap-2'>
                  <span className='text-muted-foreground text-xs'>
                    {t('{{n}} model(s) selected', { n: selectedModels.size })}
                  </span>
                  <div className='relative w-24 shrink-0'>
                    <Input
                      type='number'
                      min='0.01'
                      max='100'
                      step='0.01'
                      value={batchDiscount}
                      disabled={mutation.isPending}
                      aria-label={t('Batch discount percentage')}
                      aria-invalid={Boolean(batchError)}
                      aria-describedby={
                        batchError ? 'model-pricing-batch-error' : undefined
                      }
                      className='pr-7 font-mono'
                      onChange={(event) => {
                        setBatchDiscount(event.target.value)
                        setBatchError(null)
                      }}
                      onKeyDown={(event) => {
                        if (event.key !== 'Enter') return
                        event.preventDefault()
                        applyBatchDiscount()
                      }}
                    />
                    <span className='text-muted-foreground pointer-events-none absolute top-1/2 right-2 -translate-y-1/2 text-xs'>
                      %
                    </span>
                  </div>
                  <Button
                    type='button'
                    variant='outline'
                    disabled={mutation.isPending}
                    onClick={applyBatchDiscount}
                  >
                    <BadgePercent aria-hidden='true' />
                    {t('Apply discount')}
                  </Button>
                  <Button
                    type='button'
                    variant='ghost'
                    size='icon-sm'
                    aria-label={t('Clear selection')}
                    title={t('Clear selection')}
                    disabled={mutation.isPending}
                    onClick={() => {
                      setSelectedModels(new Set())
                      setBatchError(null)
                    }}
                  >
                    <X aria-hidden='true' />
                  </Button>
                </div>
                {batchError && (
                  <p
                    id='model-pricing-batch-error'
                    role='alert'
                    className='text-destructive text-xs'
                  >
                    {batchError}
                  </p>
                )}
              </div>
            )}

            <div className='grid shrink-0 grid-cols-[2rem_minmax(0,1fr)_7rem] items-center gap-2 px-1 text-xs font-medium sm:grid-cols-[2rem_minmax(0,1fr)_9rem]'>
              <Checkbox
                className='justify-self-center'
                aria-label={t('Select all (filtered)')}
                title={t('Select all (filtered)')}
                checked={allFilteredSelected}
                indeterminate={
                  selectedFilteredCount > 0 && !allFilteredSelected
                }
                disabled={mutation.isPending || totalFilteredModels === 0}
                onCheckedChange={(checked) => {
                  setSelectedModels((previous) => {
                    const next = new Set(previous)
                    for (const { model } of filteredRows) {
                      if (checked) next.add(model.model_name)
                      else next.delete(model.model_name)
                    }
                    return next
                  })
                }}
              />
              <span>{t('Model')}</span>
              <span>{t('Discount percentage')}</span>
            </div>

            {totalFilteredModels === 0 ? (
              <div
                data-slot='model-pricing-list'
                className='text-muted-foreground flex min-h-0 items-center justify-center rounded-md border px-4 py-10 text-center text-sm'
              >
                {totalModels === 0
                  ? t('No models available')
                  : t('No models matched your search.')}
              </div>
            ) : (
              <>
                <div
                  data-slot='model-pricing-list'
                  className='flex min-h-0 flex-col overflow-hidden rounded-md border'
                >
                  <div
                    data-slot='model-pricing-scroll'
                    className='max-h-[min(55vh,32rem)] min-h-0 flex-1 overflow-y-auto overscroll-contain'
                  >
                    <div className='divide-y'>
                      {paginatedRows.map(({ model, index }) => (
                        <div
                          key={model.model_name}
                          className='grid grid-cols-[2rem_minmax(0,1fr)_7rem] items-start gap-2 sm:grid-cols-[2rem_minmax(0,1fr)_9rem]'
                        >
                          <Checkbox
                            className='mt-3 justify-self-center'
                            aria-label={t('Select model {{model}}', {
                              model: model.model_name,
                            })}
                            checked={selectedModels.has(model.model_name)}
                            disabled={mutation.isPending}
                            onCheckedChange={(checked) => {
                              setSelectedModels((previous) => {
                                const next = new Set(previous)
                                if (checked) next.add(model.model_name)
                                else next.delete(model.model_name)
                                return next
                              })
                            }}
                          />
                          <div className='min-w-0 py-2.5'>
                            <span
                              className='block truncate font-mono text-sm'
                              title={
                                model.aliases.length > 0
                                  ? model.aliases.join(', ')
                                  : model.model_name
                              }
                            >
                              {model.model_name}
                            </span>
                            {model.aliases.length > 0 && (
                              <span className='text-muted-foreground block truncate text-xs'>
                                {model.aliases.join(', ')}
                              </span>
                            )}
                          </div>

                          <FormField
                            control={form.control}
                            name={`items.${index}.discount_percent`}
                            render={({ field }) => (
                              <FormItem className='p-2'>
                                <FormLabel className='sr-only'>
                                  {t('Discount percentage')}
                                </FormLabel>
                                <FormControl>
                                  <div className='relative'>
                                    <Input
                                      type='number'
                                      min='0.01'
                                      max='100'
                                      step='0.01'
                                      className='pr-7 font-mono'
                                      value={field.value ?? ''}
                                      onBlur={field.onBlur}
                                      onChange={(event) => {
                                        // RHF 受控字段不接受 undefined，空值使用 null 才能真正清除规则。
                                        const nextValue =
                                          event.target.valueAsNumber
                                        field.onChange(
                                          Number.isFinite(nextValue)
                                            ? nextValue
                                            : null
                                        )
                                      }}
                                    />
                                    <span className='text-muted-foreground pointer-events-none absolute top-1/2 right-2 -translate-y-1/2 text-xs'>
                                      %
                                    </span>
                                  </div>
                                </FormControl>
                                <FormMessage />
                              </FormItem>
                            )}
                          />
                        </div>
                      ))}
                    </div>
                  </div>
                </div>
                {/* 分页栏独立于模型列表，模型滚动时不会与模型行处于同一层级。 */}
                <div
                  data-slot='model-pricing-pagination'
                  className='flex shrink-0 items-center justify-between gap-2 px-1 text-xs'
                >
                  <span className='text-muted-foreground'>
                    {t('Page {{current}} of {{total}}', {
                      current: safeCurrentPage,
                      total: totalPages,
                    })}
                  </span>
                  {showPagination && (
                    <div className='flex items-center gap-1'>
                      <Button
                        type='button'
                        variant='outline'
                        size='icon-sm'
                        onClick={() =>
                          setCurrentPage((page) => Math.max(1, page - 1))
                        }
                        disabled={safeCurrentPage === 1}
                        aria-label={t('Previous page')}
                      >
                        <ChevronLeft aria-hidden='true' />
                      </Button>
                      <Button
                        type='button'
                        variant='outline'
                        size='icon-sm'
                        onClick={() =>
                          setCurrentPage((page) =>
                            Math.min(totalPages, page + 1)
                          )
                        }
                        disabled={safeCurrentPage === totalPages}
                        aria-label={t('Next page')}
                      >
                        <ChevronRight aria-hidden='true' />
                      </Button>
                    </div>
                  )}
                </div>
              </>
            )}
          </form>
        </Form>
      )}
    </Dialog>
  )
}
