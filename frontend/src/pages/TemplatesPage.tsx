/**
 * 邮件模板页（邮件模板功能包）：模板列表 + 新建/编辑（变量提示：内容占位符实时
 * 提取展示）+ 桌面/移动双预览（iframe sandbox 渲染 + 样本变量填充）+ 删除。
 * 数据：GET/POST /api/templates、PUT/DELETE /api/templates/:id（SPEC §5 风格）。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Eye, LayoutTemplate, Pencil, Plus } from 'lucide-react'
import { useMemo, useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { PageHeader } from '@/components/PageHeader'
import { QueryError, QueryLoading } from '@/components/QueryState'
import { RelativeTime } from '@/components/RelativeTime'
import { Button } from '@/components/ui/Button'
import { Card } from '@/components/ui/Card'
import { EmptyState } from '@/components/ui/EmptyState'
import { Field, Input, Textarea } from '@/components/ui/Input'
import { Modal } from '@/components/ui/Modal'
import { TD, TH, TBody, THead, TRow, Table, TableScroll } from '@/components/ui/Table'
import { api } from '@/lib/api'
import {
  buildPreviewDoc,
  extractTemplateVars,
  invalidateTemplates,
  parseSampleVars,
  renderTemplate,
  useTemplates,
  type TemplateDetail,
  type TemplateItem,
} from '@/lib/templates'

/** 可编辑的模板内容形态（编辑弹窗/预览共用；编辑态额外携带 id） */
interface TemplateForm {
  id?: number
  name: string
  subject: string
  html: string
  text: string
  variablesText: string // 显式变量清单（逗号/换行分隔；空 = 自动提取）
}

/** 服务端错误内联提示（重名/校验失败等 400 响应） */
function ServerError({ message }: { message: string }) {
  return (
    <p className="flex items-start gap-1.5 text-sm text-red-600 dark:text-red-400">
      <AlertCircleIcon />
      {message}
    </p>
  )
}

function AlertCircleIcon() {
  return (
    <svg
      className="mt-0.5 h-4 w-4 shrink-0"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <circle cx="12" cy="12" r="10" />
      <line x1="12" y1="8" x2="12" y2="12" />
      <line x1="12" y1="16" x2="12.01" y2="16" />
    </svg>
  )
}

/* ---------------------------------- 变量提示 ---------------------------------- */

/** 变量 chips：从内容占位符实时提取（与后端自动提取同口径），空清单时给示例提示 */
function VariableChips({ vars }: { vars: string[] }) {
  const { t } = useTranslation(['templates'])
  if (vars.length === 0) {
    return (
      <p className="text-xs text-slate-400 dark:text-slate-500">
        {t('vars.emptyHint', { sample: `{{${t('vars.exampleName')}}}` })}
      </p>
    )
  }
  return (
    <div className="flex flex-wrap gap-1.5">
      {vars.map((v) => (
        <code
          key={v}
          title={t('vars.chipTitle')}
          className="rounded-md bg-slate-100 px-1.5 py-0.5 font-mono text-xs text-slate-600 dark:bg-slate-800 dark:text-slate-300"
        >
          {v}
        </code>
      ))}
    </div>
  )
}

/* ---------------------------------- 双端预览 ---------------------------------- */

/**
 * 桌面/移动双预览弹窗：样本变量 JSON 填充后经 iframe sandbox（无脚本）渲染。
 * 桌面视图自适应宽度，移动视图固定 375px 手机宽度并排展示；仅 html 或仅 text 的
 * 模板均可用（text 以 <pre> 呈现）。
 */
function PreviewModal({
  title,
  subject,
  html,
  text,
  onClose,
}: {
  title: string
  subject: string
  html: string
  text: string
  onClose: () => void
}) {
  const { t } = useTranslation(['templates', 'common'])
  const [sampleText, setSampleText] = useState('')
  const sampleVars = useMemo(() => parseSampleVars(sampleText), [sampleText])
  const doc = useMemo(
    () => buildPreviewDoc({ html_body: html, text_body: text }, sampleVars ?? {}),
    [html, text, sampleVars],
  )
  const renderedSubject = useMemo(() => renderTemplate(subject, sampleVars ?? {}, false), [subject, sampleVars])

  return (
    <Modal
      open
      onClose={onClose}
      title={t('preview.title', { name: title })}
      className="max-w-5xl"
      footer={
        <Button variant="secondary" onClick={onClose}>
          {t('common:actions.close')}
        </Button>
      }
    >
      <div className="space-y-4">
        <Field
          label={t('preview.sampleVarsLabel')}
          hint={t('preview.sampleVarsHint')}
          error={sampleVars === null ? t('preview.sampleVarsInvalid') : undefined}
        >
          <Textarea
            value={sampleText}
            onChange={(e) => setSampleText(e.target.value)}
            placeholder={t('preview.sampleVarsPlaceholder')}
            className="min-h-16 font-mono text-xs"
            spellCheck={false}
          />
        </Field>
        <div className="flex flex-wrap gap-x-2 gap-y-1 text-sm text-slate-600 dark:text-slate-300">
          <span className="font-medium">{t('preview.renderedSubjectLabel')}</span>
          <span className="break-all">{renderedSubject || t('preview.emptySubject')}</span>
        </div>
        <div className="flex flex-wrap items-start gap-4">
          {/* 桌面视图：自适应宽度 */}
          <div className="min-w-[280px] flex-1">
            <p className="mb-1.5 text-xs font-medium text-slate-500 dark:text-slate-400">{t('preview.desktopView')}</p>
            <div className="overflow-hidden rounded-xl border border-slate-200 dark:border-slate-700">
              <iframe
                title={t('preview.desktopIframeTitle')}
                sandbox=""
                srcDoc={doc}
                className="h-80 w-full bg-white"
              />
            </div>
          </div>
          {/* 移动视图：375px 手机宽度 */}
          <div className="w-[375px] max-w-full">
            <p className="mb-1.5 text-xs font-medium text-slate-500 dark:text-slate-400">{t('preview.mobileView')}</p>
            <div className="overflow-hidden rounded-xl border-4 border-slate-200 dark:border-slate-700">
              <iframe
                title={t('preview.mobileIframeTitle')}
                sandbox=""
                srcDoc={doc}
                className="h-80 w-full bg-white"
              />
            </div>
          </div>
        </div>
      </div>
    </Modal>
  )
}

/* ---------------------------------- 新建 / 编辑 ---------------------------------- */

function TemplateFormModal({ initial, onClose }: { initial: TemplateForm | null; onClose: () => void }) {
  const { t } = useTranslation(['templates', 'common'])
  const isEdit = initial?.id !== undefined
  const [name, setName] = useState(initial?.name ?? '')
  const [subject, setSubject] = useState(initial?.subject ?? '')
  const [html, setHtml] = useState(initial?.html ?? '')
  const [text, setText] = useState(initial?.text ?? '')
  const [variablesText, setVariablesText] = useState(initial?.variablesText ?? '')
  const [previewOpen, setPreviewOpen] = useState(false)
  const [errors, setErrors] = useState<{ name?: string; subject?: string; body?: string; variables?: string }>({})
  const [serverError, setServerError] = useState<string | null>(null)
  const queryClient = useQueryClient()

  // 变量提示：内容占位符实时提取（保存时未显式提供清单则由后端按内容自动提取）
  const extractedVars = useMemo(() => extractTemplateVars(subject, html, text), [subject, html, text])
  const manualVars = useMemo(
    () => variablesText.split(/[,，\n]/).map((s) => s.trim()).filter(Boolean),
    [variablesText],
  )
  const hasManual = manualVars.length > 0
  // {{...}} 是模板语法而非插值变量：示例占位符经 sample 参数原样传入，避免被 i18next 吞掉
  const varExample = `{{${t('vars.exampleName')}}}`
  const varExampleShort = `{{${t('vars.exampleShort')}}}`

  const mutation = useMutation({
    mutationFn: (input: { name: string; subject: string; html: string; text: string; variables?: string[] }) =>
      isEdit
        ? api<TemplateItem>(`/api/templates/${initial!.id}`, { method: 'PUT', body: input })
        : api<TemplateItem>('/api/templates', { method: 'POST', body: input }),
    onSuccess: (tpl) => {
      toast.success(isEdit ? t('toast.saved', { name: tpl.name }) : t('toast.created', { name: tpl.name }))
      invalidateTemplates(queryClient)
      onClose()
    },
    onError: (err) => setServerError(err.message),
  })

  function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const nextErrors: typeof errors = {}
    const trimmedName = name.trim()
    const codePoints = [...trimmedName].length
    if (!trimmedName) nextErrors.name = t('form.nameRequired')
    else if (codePoints > 128) nextErrors.name = t('form.nameTooLong')
    if (!subject.trim()) nextErrors.subject = t('form.subjectRequired', { sample: varExampleShort })
    else if ([...subject.trim()].length > 998) nextErrors.subject = t('form.subjectTooLong')
    if (!html.trim() && !text.trim()) nextErrors.body = t('form.bodyRequired')
    // 显式清单解析失败（空名单但输入了分隔符之外的垃圾内容不会发生）——只校验数量
    if (manualVars.length > 100) nextErrors.variables = t('form.varsListTooMany')
    setErrors(nextErrors)
    if (Object.values(nextErrors).some(Boolean)) return
    setServerError(null)
    mutation.mutate({
      name: trimmedName,
      subject: subject.trim(),
      html,
      text,
      // 留空 = 交由后端按内容自动提取；非空 = 以显式清单为准
      ...(hasManual ? { variables: manualVars } : {}),
    })
  }

  return (
    <>
      <Modal
        open
        onClose={onClose}
        title={isEdit ? t('form.editTitle') : t('form.createTitle')}
        className="max-w-2xl"
        footer={
          <>
            <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
              {t('common:actions.cancel')}
            </Button>
            <Button variant="secondary" onClick={() => setPreviewOpen(true)} disabled={mutation.isPending}>
              <Eye className="h-4 w-4" aria-hidden />
              {t('actions.preview')}
            </Button>
            <Button type="submit" form="template-form" loading={mutation.isPending}>
              {isEdit ? t('common:actions.save') : t('form.submitCreate')}
            </Button>
          </>
        }
      >
        <form id="template-form" onSubmit={handleSubmit} className="space-y-4" noValidate>
          {serverError && <ServerError message={serverError} />}
          <Field label={t('form.name')} error={errors.name} hint={t('form.nameHint')}>
            <Input
              name="name"
              value={name}
              onChange={(e) => {
                setName(e.target.value)
                setErrors((prev) => ({ ...prev, name: undefined }))
                setServerError(null)
              }}
              placeholder={t('form.namePlaceholder')}
              autoFocus
              autoComplete="off"
            />
          </Field>
          <Field label={t('form.subject')} error={errors.subject} hint={t('form.subjectHint', { sample: varExample })}>
            <Input
              name="subject"
              value={subject}
              onChange={(e) => {
                setSubject(e.target.value)
                setErrors((prev) => ({ ...prev, subject: undefined }))
                setServerError(null)
              }}
              placeholder="欢迎 {{name}} 加入"
              autoComplete="off"
            />
          </Field>
          <Field label={t('form.htmlBody')} error={errors.body} hint={t('form.htmlBodyHint')}>
            <Textarea
              name="html"
              value={html}
              onChange={(e) => {
                setHtml(e.target.value)
                setErrors((prev) => ({ ...prev, body: undefined }))
                setServerError(null)
              }}
              placeholder={'<p>您好 {{name}}，您的验证码是 {{code}}。</p>'}
              className="min-h-28 font-mono text-xs"
              spellCheck={false}
            />
          </Field>
          <Field label={t('form.textBody')} hint={t('form.textBodyHint')}>
            <Textarea
              name="text"
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder="您好 {{name}}，您的验证码是 {{code}}。"
              className="min-h-16 font-mono text-xs"
              spellCheck={false}
            />
          </Field>
          <Field
            label={t('form.varsLabel')}
            labelExtra={<span className="text-xs text-slate-400">{t('form.varsLabelExtra')}</span>}
          >
            <VariableChips vars={extractedVars} />
          </Field>
          <Field
            label={t('form.varsListLabel')}
            error={errors.variables}
            hint={t('form.varsListHint')}
          >
            <Textarea
              name="variables"
              value={variablesText}
              onChange={(e) => {
                setVariablesText(e.target.value)
                setErrors((prev) => ({ ...prev, variables: undefined }))
              }}
              placeholder="name, code"
              className="min-h-12 font-mono text-xs"
              spellCheck={false}
            />
          </Field>
        </form>
      </Modal>
      {previewOpen && (
        <PreviewModal
          title={name || t('form.untitled')}
          subject={subject}
          html={html}
          text={text}
          onClose={() => setPreviewOpen(false)}
        />
      )}
    </>
  )
}

/* ---------------------------------- 删除确认 ---------------------------------- */

function DeleteTemplateModal({ tpl, onClose }: { tpl: TemplateItem; onClose: () => void }) {
  const { t } = useTranslation(['templates', 'common'])
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => api<{ ok: boolean }>(`/api/templates/${tpl.id}`, { method: 'DELETE' }),
    onSuccess: () => {
      toast.success(t('toast.deleted', { name: tpl.name }))
      invalidateTemplates(queryClient)
      onClose()
    },
    onError: (err) => toast.error(err.message),
  })

  return (
    <Modal
      open
      onClose={onClose}
      title={t('delete.title')}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            {t('common:actions.cancel')}
          </Button>
          <Button variant="danger" loading={mutation.isPending} onClick={() => mutation.mutate()}>
            {t('delete.confirm')}
          </Button>
        </>
      }
    >
      <p className="text-sm text-slate-600 dark:text-slate-300">
        {t('delete.confirmPrefix')}{' '}
        <span className="font-medium text-slate-900 dark:text-white">
          {t('delete.nameWrapped', { name: tpl.name })}
        </span> {t('delete.confirmSuffix')}
      </p>
      <p className="mt-2 text-xs text-slate-500 dark:text-slate-400">{t('delete.warning')}</p>
    </Modal>
  )
}

/* ---------------------------------- 页面 ---------------------------------- */

export function TemplatesPage() {
  const { t } = useTranslation(['templates', 'common'])
  const templatesQuery = useTemplates()
  const [formTarget, setFormTarget] = useState<TemplateForm | null>(null)
  const [previewTarget, setPreviewTarget] = useState<TemplateItem | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<TemplateItem | null>(null)

  const items = templatesQuery.data?.items ?? []

  return (
    <div className="space-y-6">
      <PageHeader
        title={t('page.title')}
        description={t('page.description', { sample: '{{var}}' })}
        actions={
          <Button onClick={() => setFormTarget({ name: '', subject: '', html: '', text: '', variablesText: '' })}>
            <Plus className="h-4 w-4" aria-hidden />
            {t('page.create')}
          </Button>
        }
      />

      <Card>
        {templatesQuery.isPending ? (
          <QueryLoading skeleton="table" />
        ) : templatesQuery.isError ? (
          <QueryError message={templatesQuery.error.message} onRetry={() => templatesQuery.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState
            icon={LayoutTemplate}
            title={t('page.emptyTitle')}
            description={t('page.emptyDescription')}
            action={
              <Button
                onClick={() => setFormTarget({ name: '', subject: '', html: '', text: '', variablesText: '' })}
              >
                <Plus className="h-4 w-4" aria-hidden />
                {t('page.create')}
              </Button>
            }
          />
        ) : (
          <TableScroll label={t('table.label')}>
            <Table label={t('table.label')}>
              <THead>
                <tr>
                  <TH>{t('common:fields.name')}</TH>
                  <TH>{t('table.subject')}</TH>
                  <TH>{t('table.variables')}</TH>
                  <TH>{t('table.updatedAt')}</TH>
                  <TH className="text-right">{t('common:fields.actions')}</TH>
                </tr>
              </THead>
              <TBody>
                {items.map((tpl) => (
                  <TRow key={tpl.id}>
                    <TD>
                      <span className="font-medium text-slate-900 dark:text-white">{tpl.name}</span>
                      <span className="ml-2 font-mono text-xs text-slate-400">#{tpl.id}</span>
                    </TD>
                    <TD>
                      <span className="line-clamp-1 max-w-[280px] break-all text-slate-600 dark:text-slate-300">
                        {tpl.subject}
                      </span>
                    </TD>
                    <TD>
                      {(tpl.variables?.length ?? 0) > 0 ? (
                        <div className="flex max-w-[240px] flex-wrap gap-1">
                          {tpl.variables!.slice(0, 4).map((v) => (
                            <code
                              key={v}
                              className="rounded-md bg-slate-100 px-1.5 py-0.5 font-mono text-xs text-slate-600 dark:bg-slate-800 dark:text-slate-300"
                            >
                              {v}
                            </code>
                          ))}
                          {tpl.variables!.length > 4 && (
                            <span className="text-xs text-slate-400">+{tpl.variables!.length - 4}</span>
                          )}
                        </div>
                      ) : (
                        <span className="text-slate-400">—</span>
                      )}
                    </TD>
                    <TD>
                      <RelativeTime iso={tpl.updated_at} />
                    </TD>
                    <TD className="text-right">
                      <div className="flex items-center justify-end gap-1">
                        <Button
                          variant="ghost"
                          className="h-7 px-2 text-xs"
                          onClick={() => setPreviewTarget(tpl)}
                        >
                          <Eye className="h-3.5 w-3.5" aria-hidden />
                          {t('actions.preview')}
                        </Button>
                        <Button
                          variant="ghost"
                          className="h-7 px-2 text-xs"
                          onClick={() =>
                            setFormTarget({
                              id: tpl.id,
                              name: tpl.name,
                              subject: tpl.subject,
                              // 编辑正文需要全量内容：懒加载详情（见 formTarget 渲染分支）
                              html: '',
                              text: '',
                              variablesText: '',
                            })
                          }
                        >
                          <Pencil className="h-3.5 w-3.5" aria-hidden />
                          {t('common:actions.edit')}
                        </Button>
                        <button
                          type="button"
                          onClick={() => setDeleteTarget(tpl)}
                          className="inline-flex h-7 items-center rounded-lg px-2 text-xs text-red-600 transition-colors hover:bg-red-50 hover:text-red-700 dark:text-red-400 dark:hover:bg-red-500/10"
                        >
                          {t('common:actions.delete')}
                        </button>
                      </div>
                    </TD>
                  </TRow>
                ))}
              </TBody>
            </Table>
          </TableScroll>
        )}
      </Card>

      {/* 编辑需要正文大字段：打开编辑弹窗时先取详情再渲染表单 */}
      {formTarget?.id !== undefined ? (
        <TemplateEditLoader id={formTarget.id} onClose={() => setFormTarget(null)} />
      ) : (
        formTarget && <TemplateFormModal initial={formTarget} onClose={() => setFormTarget(null)} />
      )}
      {previewTarget && (
        <PreviewLoader id={previewTarget.id} name={previewTarget.name} onClose={() => setPreviewTarget(null)} />
      )}
      {deleteTarget && (
        <DeleteTemplateModal key={deleteTarget.id} tpl={deleteTarget} onClose={() => setDeleteTarget(null)} />
      )}
    </div>
  )
}

/** 编辑弹窗装载器：正文大字段不在列表响应内，编辑前按 id 拉取详情 */
function TemplateEditLoader({ id, onClose }: { id: number; onClose: () => void }) {
  const { t } = useTranslation(['templates'])
  const detail = useTemplateDetail(id)
  if (detail === undefined) {
    return (
      <Modal open onClose={onClose} title={t('form.editTitle')}>
        <QueryLoading skeleton="block" />
      </Modal>
    )
  }
  return (
    <TemplateFormModal
      initial={{
        id: detail.id,
        name: detail.name,
        subject: detail.subject,
        html: detail.html_body,
        text: detail.text_body,
        variablesText: (detail.variables ?? []).join(', '),
      }}
      onClose={onClose}
    />
  )
}

/** 预览弹窗装载器：从详情接口取正文后渲染双端预览 */
function PreviewLoader({ id, name, onClose }: { id: number; name: string; onClose: () => void }) {
  const { t } = useTranslation(['templates'])
  const detail = useTemplateDetail(id)
  if (detail === undefined) {
    return (
      <Modal open onClose={onClose} title={t('actions.preview')}>
        <QueryLoading skeleton="block" />
      </Modal>
    )
  }
  return (
    <PreviewModal
      title={name}
      subject={detail.subject}
      html={detail.html_body}
      text={detail.text_body}
      onClose={onClose}
    />
  )
}

/** GET /api/templates/:id 单次详情（组件级缓存键隔离） */
function useTemplateDetail(id: number): TemplateDetail | undefined {
  const { data } = useQuery({
    queryKey: ['templates', 'detail', id],
    queryFn: () => api<TemplateDetail>(`/api/templates/${id}`),
  })
  return data
}
