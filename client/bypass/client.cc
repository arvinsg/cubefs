#include "client.h"

/*
 * File operations
 */

static int close_cfs_fd(int fd) {
    bool release_file = false;
    bool release_inode = false;

    pthread_rwlock_wrlock(&g_client_info.dup_fds_lock);
    auto dup_fd_it = g_client_info.dup_fds.find(fd);
    if(dup_fd_it != g_client_info.dup_fds.end()) {
        fd = dup_fd_it->second;
        g_client_info.dup_fds.erase(dup_fd_it);
    } else {
        fd = fd & ~CFS_FD_MASK;
    }
    pthread_rwlock_unlock(&g_client_info.dup_fds_lock);

    pthread_rwlock_wrlock(&g_client_info.open_files_lock);
    auto open_file_it = g_client_info.open_files.find(fd);
    if(open_file_it == g_client_info.open_files.end()) {
        pthread_rwlock_unlock(&g_client_info.open_files_lock);
        return 0;
    }
    file_t *f = open_file_it->second;
    pthread_mutex_lock(&f->file_lock);
    f->dup_ref--;
    if(f->dup_ref == 0) {
        release_file = true;
    }
    pthread_mutex_unlock(&f->file_lock);
    if(!release_file) {
        pthread_rwlock_unlock(&g_client_info.open_files_lock);
        return 0;
    }

    if(strlen(g_client_info.replicate_path) > 0) {
        int re = libc_close(fd);
        if(re < 0) {
            return re;
        }
    }

    inode_info_t* inode_info = f->inode_info;
    f->inode_info = NULL;
    pthread_mutex_destroy(&f->file_lock);
    g_client_info.open_files.erase(open_file_it);
    free(f);
    pthread_rwlock_unlock(&g_client_info.open_files_lock);

    pthread_rwlock_wrlock(&g_client_info.open_inodes_lock);
    pthread_mutex_lock(&inode_info->inode_lock);
    inode_info->fd_ref--;

    if (inode_info->fd_ref == 0) {
        g_client_info.open_inodes.erase(inode_info->inode);
        release_inode = true;
    }
    pthread_mutex_unlock(&inode_info->inode_lock);
    pthread_rwlock_unlock(&g_client_info.open_inodes_lock);

    if (release_inode) {
        flush_inode(inode_info);
        release_inode_info(inode_info);
    }
    return cfs_errno(cfs_close(g_client_info.cfs_client_id, fd));
}

int real_close(int fd) {
    if(fd < 0) {
        return -1;
    }
    int re = -1;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        re = close_cfs_fd(fd);
    } else {
        re = libc_close(fd);
    }

    #if defined(_CFS_DEBUG) || defined(DUP_TO_LOCAL)
    fd = fd & ~CFS_FD_MASK;
    pthread_rwlock_wrlock(&g_client_info.fd_path_lock);
    auto it = g_client_info.fd_path.find(fd);
    #if defined(_CFS_DEBUG)
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, re:%d\n", __func__, is_cfs, fd, it != g_client_info.fd_path.end() ? it->second : "", re);
    #endif
    if(it != g_client_info.fd_path.end()) {
        free(it->second);
        g_client_info.fd_path.erase(it);
    }
    pthread_rwlock_unlock(&g_client_info.fd_path_lock);
    #endif
    return re;
}

static inode_info_t *record_inode_info(ino_t inode, int file_type, size_t size) {
    inode_info_t *inode_info = NULL;

    pthread_rwlock_rdlock(&g_client_info.open_inodes_lock);
    auto it = g_client_info.open_inodes.find(inode);
    if(it != g_client_info.open_inodes.end()) {
        inode_info = it->second;
        pthread_mutex_lock(&inode_info->inode_lock);
        inode_info->fd_ref++;
        pthread_mutex_unlock(&inode_info->inode_lock);
    }
    pthread_rwlock_unlock(&g_client_info.open_inodes_lock);

    if(inode_info != NULL) {
        return inode_info;
    }

    bool use_pagecache = false;
    if(file_type == FILE_TYPE_RELAY_LOG || file_type == FILE_TYPE_BIN_LOG) {
        use_pagecache = true;
    }
    inode_info = new_inode_info(inode, use_pagecache, cfs_pwrite_inode);
    if(inode_info == NULL) {
        return NULL;
    }
    inode_info->client_id = g_client_info.cfs_client_id;
    inode_info->size = size;

    if (use_pagecache) {
        if(file_type == FILE_TYPE_BIN_LOG || file_type == FILE_TYPE_RELAY_LOG) {
            inode_info->c = g_client_info.big_page_cache;
        } else {
            inode_info->c = g_client_info.small_page_cache;
        }
        inode_info->cache_flag |= FILE_CACHE_WRITE_BACK;
        if(file_type == FILE_TYPE_RELAY_LOG) {
            inode_info->cache_flag |= FILE_CACHE_PRIORITY_HIGH;
        }
    }

    pthread_rwlock_wrlock(&g_client_info.open_inodes_lock);
    it = g_client_info.open_inodes.find(inode);
    if(it != g_client_info.open_inodes.end()) {
        release_inode_info(inode_info);
        inode_info = it->second;
        pthread_mutex_lock(&inode_info->inode_lock);
        inode_info->fd_ref++;
        pthread_mutex_unlock(&inode_info->inode_lock);
        pthread_rwlock_unlock(&g_client_info.open_inodes_lock);
        return inode_info;
    }
    g_client_info.open_inodes[inode] = inode_info;
    pthread_rwlock_unlock(&g_client_info.open_inodes_lock);
    return inode_info;
}

static int record_open_file(cfs_file_t *cfs_file) {
    file_t *f = (file_t *)calloc(1, sizeof(file_t));
    if(f == NULL) {
        fprintf(stderr, "calloc file_t failed.\n");
        return -1;
    }
    f->fd = cfs_file->fd;
    f->file_type = cfs_file->file_type;
    f->flags = cfs_file->flags;
    f->pos = cfs_file->pos;
    f->dup_ref = cfs_file->dup_ref;
    pthread_mutex_init(&f->file_lock, NULL);

    inode_info_t *inode_info = record_inode_info(cfs_file->inode, cfs_file->file_type, cfs_file->size);
    if (inode_info == NULL) {
        free(f);
        return -1;
    }
    f->inode_info = inode_info;

    pthread_rwlock_wrlock(&g_client_info.open_files_lock);
    g_client_info.open_files[f->fd] = f;
    pthread_rwlock_unlock(&g_client_info.open_files_lock);
    return 0;
}

int real_openat(int dirfd, const char *pathname, int flags, ...) {
    mode_t mode = 0;
    if(flags & O_CREAT) {
        va_list args;
        va_start(args, flags);
        mode = va_arg(args, mode_t);
        va_end(args);
    }

    bool is_cfs = false;
    char *path = NULL;
    char *local_path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        local_path = cat_path(g_client_info.replicate_path, path);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs) {
            dirfd = get_cfs_fd(dirfd);
        }
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int fd, fd_origin = -1;
    if(g_hook && is_cfs) {
        if(strlen(g_client_info.replicate_path) > 0) {
            fd = libc_openat(dirfd, local_path != NULL ? local_path : pathname, flags, mode);
            if(fd < 0) {
                goto log;
            }
            fd = cfs_errno(cfs_openat_fd(g_client_info.cfs_client_id, dirfd, cfs_path, flags, mode, fd));
        } else {
            fd = cfs_errno(cfs_openat(g_client_info.cfs_client_id, dirfd, cfs_path, flags, mode));
        }
        if(fd < 0) {
            goto log;
        }
    } else {
        fd = libc_openat(dirfd, pathname, flags, mode);
    }

    if(fd > 0 && fd & CFS_FD_MASK) {
        if(g_hook && is_cfs) {
            cfs_close(g_client_info.cfs_client_id, fd);
        } else {
            libc_close(fd);
        }
        fd = -1;
    }
    fd_origin = fd;
    if(g_hook && is_cfs && fd > 0) {
        cfs_file_t cfs_file;
        cfs_get_file(g_client_info.cfs_client_id, fd, &cfs_file);
        if(record_open_file(&cfs_file) < 0) {
            fprintf(stderr, "cache open_file %d failed.\n", fd);
            fd = -1;
        } else {
            fd |= CFS_FD_MASK;
        }
    }

log:
    free(path);
    free(local_path);
    #if defined(_CFS_DEBUG) || defined(DUP_TO_LOCAL)
    pthread_rwlock_wrlock(&g_client_info.fd_path_lock);
    g_client_info.fd_path[fd_origin] = strdup(pathname);
    pthread_rwlock_unlock(&g_client_info.fd_path_lock);
    #endif
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, dirfd:%d, pathname:%s, flags:%#x(%s%s%s%s%s%s%s), re:%d\n",
    __func__, is_cfs, dirfd, pathname, flags, flags&O_RDONLY?"O_RDONLY|":"",
    flags&O_WRONLY?"O_WRONLY|":"", flags&O_RDWR?"O_RDWR|":"", flags&O_CREAT?"O_CREAT|":"",
    flags&O_DIRECT?"O_DIRECT|":"", flags&O_SYNC?"O_SYNC|":"", flags&O_DSYNC?"O_DSYNC":"", fd_origin);
    #endif
    return fd;
}

int real_renameat(int olddirfd, const char *old_pathname,
        int newdirfd, const char *new_pathname) {
    return real_renameat2(olddirfd, old_pathname, newdirfd, new_pathname, 0);
}

// rename between cfs and ordinary file is not allowed
int real_renameat2(int olddirfd, const char *old_pathname,
        int newdirfd, const char *new_pathname, unsigned int flags) {
    bool is_cfs_old = false;
    char *old_path = NULL;
    char *local_old_path = NULL;
    int libc_oldfd = olddirfd;
    if((old_pathname != NULL && old_pathname[0] == '/') || olddirfd == AT_FDCWD) {
        old_path = get_cfs_path(old_pathname);
        local_old_path = cat_path(g_client_info.replicate_path, old_path);
        is_cfs_old = (old_path != NULL);
    } else {
        is_cfs_old = fd_in_cfs(olddirfd);
        if(is_cfs_old){
            olddirfd = get_cfs_fd(olddirfd);
        }
    }

    bool is_cfs_new = false;
    char *new_path = NULL;
    char *local_new_path = NULL;
    int libc_newfd = newdirfd;
    if((new_pathname != NULL && new_pathname[0] == '/') || newdirfd == AT_FDCWD) {
        new_path = get_cfs_path(new_pathname);
        local_new_path = cat_path(g_client_info.replicate_path, new_path);
        is_cfs_new = (new_path != NULL);
    } else {
        is_cfs_new = fd_in_cfs(newdirfd);
        if(is_cfs_new) {
            newdirfd = get_cfs_fd(newdirfd);
        }
    }

    const char *cfs_old_path = (old_path == NULL) ? old_pathname : old_path;
    const char *cfs_new_path = (new_path == NULL) ? new_pathname : new_path;
    int re = -1;
    if(g_hook && is_cfs_old && is_cfs_new) {
        if(flags & RENAME_NOREPLACE) {
            if(!cfs_faccessat(g_client_info.cfs_client_id, newdirfd, cfs_new_path, F_OK, 0)) {
                errno = ENOTEMPTY;
                goto log;
            }
        } else if(flags) {
            // other flags unimplemented
            goto log;
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            if(libc_oldfd != AT_FDCWD && libc_oldfd & CFS_FD_MASK) {
                libc_oldfd = olddirfd;
            }
            if(libc_newfd != AT_FDCWD && libc_newfd & CFS_FD_MASK) {
                libc_newfd = newdirfd;
            }
            const char *local_old = (local_old_path != NULL ? local_old_path : old_pathname);
            const char *local_new = (local_new_path != NULL ? local_new_path : new_pathname);
            if(g_client_info.has_renameat2) {
                re = libc_renameat2(libc_oldfd, local_old, libc_newfd, local_new, flags);
            } else {
                re = libc_renameat(libc_oldfd, local_old, libc_newfd, local_new);
            }
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_renameat(g_client_info.cfs_client_id, olddirfd, cfs_old_path, newdirfd, cfs_new_path));
    } else if(!g_hook || (!is_cfs_old && !is_cfs_new)) {
        if(g_client_info.has_renameat2) {
            re = libc_renameat2(olddirfd, old_pathname, newdirfd, new_pathname, flags);
        } else {
            re = libc_renameat(olddirfd, old_pathname, newdirfd, new_pathname);
        }
    }

log:
    free(old_path);
    free(new_path);
    free(local_old_path);
    free(local_new_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, olddirfd:%d, old_pathname:%s, is_cfs_old:%d, newdirfd:%d, new_pathname:%s, is_cfs_new:%d, flags:%#x, re:%d\n", __func__, olddirfd, old_pathname, is_cfs_old, newdirfd, new_pathname, is_cfs_new, flags, re);
    #endif
    return re;
}

int real_truncate(const char *pathname, off_t length) {
    char *path = get_cfs_path(pathname);
    int re;
    if(g_hook && path != NULL) {
        if(strlen(g_client_info.replicate_path) > 0) {
            char *local_path = cat_path(g_client_info.replicate_path, path);
            re = libc_truncate(local_path, length);
            free(local_path);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_truncate(g_client_info.cfs_client_id, path, length));
        if(re < 0) {
            goto log;
        }
        struct stat statbuf;
        int re1 = cfs_errno(cfs_stat(g_client_info.cfs_client_id, path, &statbuf));
        if(re1 == 0) {
            inode_info_t *inode = get_open_inode(statbuf.st_ino);
            if(inode != NULL) {
                inode->size = length;
            }
        }
    } else {
        re = libc_truncate(pathname, length);
    }

log:
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, pathname:%s, length:%d, re:%d\n", __func__, path != NULL, pathname, re);
    #endif
    return re;
}

int real_ftruncate(int fd, off_t length) {
    if(fd < 0) {
        return -1;
    }
    int re = -1;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_ftruncate(fd, length);
            if(re < 0) {
                goto log;
            }
        }
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        f->inode_info->size = length;
        re = cfs_errno(cfs_ftruncate(g_client_info.cfs_client_id, fd, length));
    } else {
        re = libc_ftruncate(fd, length);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, length:%d, re:%d\n", __func__, is_cfs, fd, fd_path, length, re);
    #endif
    return re;
}

int real_fallocate(int fd, int mode, off_t offset, off_t len) {
    int re;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_fallocate(fd, mode, offset, len);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_fallocate(g_client_info.cfs_client_id, fd, mode, offset, len));
    } else {
        re = libc_fallocate(fd, mode, offset, len);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, mode:%#X, offset:%ld, len:%d, re:%d\n", __func__, is_cfs, fd, fd_path, mode, offset, len, re);
    #endif
    return re;
}

int real_posix_fallocate(int fd, off_t offset, off_t len) {
    int re;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_posix_fallocate(fd, offset, len);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_posix_fallocate(g_client_info.cfs_client_id, fd, offset, len));
    } else {
        re = libc_posix_fallocate(fd, offset, len);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, offset:%ld, len:%d, re:%d\n", __func__, is_cfs, fd, fd_path, offset, len, re);
    #endif
    return re;
}

/*
 * Directory operations
 */

int real_mkdirat(int dirfd, const char *pathname, mode_t mode) {
    bool is_cfs = false;
    char *path = NULL;
    char *local_path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        local_path = cat_path(g_client_info.replicate_path, path);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs) {
            dirfd = get_cfs_fd(dirfd);
        }
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    if(g_hook && is_cfs) {
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_mkdirat(dirfd, local_path != NULL ? local_path : pathname, mode);
            if(re < 0) {
                goto log;
            }
        }
        re =cfs_errno(cfs_mkdirsat(g_client_info.cfs_client_id, dirfd, cfs_path, mode));
    } else {
        re = libc_mkdirat(dirfd, pathname, mode);
    }

log:
    free(path);
    free(local_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, dirfd: %d, pathname:%s, mode:%d, re:%d\n", __func__, is_cfs, dirfd, pathname == NULL ? "" : pathname, mode, re);
    #endif
    return re;
}

int real_rmdir(const char *pathname) {
    char *path = get_cfs_path(pathname);
    int re;
    if(g_hook && path != NULL) {
        bool is_root = (strlen(path) == 0 || (strlen(path) == 1 && path[0] == '/'));
        if(!is_root && strlen(g_client_info.replicate_path) > 0) {
            char *local_path = cat_path(g_client_info.replicate_path, path);
            re = libc_rmdir(local_path);
            free(local_path);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_rmdir(g_client_info.cfs_client_id, path));
    } else {
        re = libc_rmdir(pathname);
    }

log:
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, pathname:%s, re:%d\n", __func__, path != NULL, pathname == NULL ? "" : pathname, re);
    #endif
    return re;
}

char *real_getcwd(char *buf, size_t size) {
    char *re = NULL;
    char *tmpcwd = NULL;
    int alloc_size;
    int len_mount;
    int len_cwd;
    int len;
    if(buf != NULL && size == 0) {
        errno = EINVAL;
        goto log;
    }

    if(g_client_info.cwd == NULL) {
        char *cwd = libc_getcwd(buf, size);
        if(cwd == NULL) {
            goto log;
        }
        // Always duplicate cwd enven if cwd is malloc 'ed by libc_getcwd,
        // because caller of getcwd may free the returned cwd afterwards.
        char *dupcwd = strdup(cwd);
        if(dupcwd == NULL) {
            if(buf == NULL) {
                free(cwd);
            }
            goto log;
        }
        g_client_info.cwd = dupcwd;
        g_client_info.in_cfs = false;
        re = cwd;
        goto log;
    }

    len_mount = 0;

    tmpcwd = strdup(g_client_info.cwd);
    // If g_client_info.cwd="/" ignore the backslash
    len_cwd = strcmp(tmpcwd, "/") ? strlen(tmpcwd) : 0;
    len = len_cwd;
    if(g_client_info.in_cfs) {
        len_mount = strlen(g_client_info.mount_point);
        len += len_mount;
    }
    if(size > 0 && size < len+1) {
        errno = ENAMETOOLONG;
        goto log;
    }

    alloc_size = size;
    if(size == 0) {
        alloc_size = len + 1;
    }
    if(buf == NULL) {
        buf = (char *)malloc(alloc_size);
        if(buf == NULL) {
            goto log;
        }
        memset(buf, '\0', alloc_size);
    }

    if(g_client_info.in_cfs) {
        strcat(buf, g_client_info.mount_point);
    }
    if(len_cwd > 0) {
        strcat(buf, tmpcwd);
    }
    re = buf;

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, re: %s\n", __func__, re == NULL ? "" : re);
    #endif
    free(tmpcwd);
    return re;
}

int real_chdir(const char *pathname) {
    int re = -1;
    char *clean_path = get_clean_path(pathname);
    char *abs_path;
    char *cfs_path;
    if(clean_path == NULL) {
        goto log;
    }

    abs_path = clean_path;
    if(pathname[0] != '/') {
        char *cwd = getcwd(NULL, 0);
        if(cwd == NULL) {
            free(clean_path);
            goto log;
        }
        abs_path = cat_path(cwd, clean_path);
        free(cwd);
        free(clean_path);
        if(abs_path == NULL) {
            goto log;
        }
    }

    cfs_path = get_cfs_path(abs_path);
    if(g_hook && cfs_path != NULL) {
        if(strlen(g_client_info.replicate_path) > 0) {
            char *local_path = cat_path(g_client_info.replicate_path, cfs_path);
            re = libc_chdir(local_path);
            free(local_path);
            if(re < 0) {
                free(abs_path);
                goto log;
            }
        }
        free(abs_path);
        re = cfs_errno(cfs_chdir(g_client_info.cfs_client_id, cfs_path));
        if(re == 0) {
            g_client_info.in_cfs = true;
            free(g_client_info.cwd);
            g_client_info.cwd = cfs_path;
        } else {
            free(cfs_path);
        }
    } else {
        free(cfs_path);
        re = libc_chdir(abs_path);
        if(re == 0) {
            g_client_info.in_cfs = false;
            free(g_client_info.cwd);
            g_client_info.cwd = abs_path;
        } else {
            free(abs_path);
        }
    }

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, pathname:%s, re:%d\n", __func__, pathname == NULL ? "" : pathname, re);
    #endif
    return re;
}

int real_fchdir(int fd) {
    int re = -1;
    char *buf;
    bool is_cfs = fd_in_cfs(fd);
    if(!g_hook || !is_cfs) {
        re = libc_fchdir(fd);
        g_client_info.in_cfs = false;
        free(g_client_info.cwd);
        g_client_info.cwd = NULL;
        goto log;
    }

    fd = get_cfs_fd(fd);
    if(strlen(g_client_info.replicate_path) > 0) {
        re = libc_fchdir(fd);
        if(re < 0) {
            goto log;
        }
    }
    buf = (char *) malloc(PATH_MAX);
    re = cfs_errno(cfs_fchdir(g_client_info.cfs_client_id, fd, buf, PATH_MAX));
    if (re == 0) {
        g_client_info.in_cfs = true;
        free(g_client_info.cwd);
        g_client_info.cwd = buf;
    } else {
        free(buf);
    }

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, fd:%d, re:%d\n", __func__, is_cfs, fd, re);
    #endif
    return re;
}

DIR *real_opendir(const char *pathname) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s, pathname:%s\n", __func__, pathname);
    #endif
    char *path = get_cfs_path(pathname);
    if(!g_hook || path == NULL) {
        free(path);
        return libc_opendir(pathname);
    }

    int fd;
    if(strlen(g_client_info.replicate_path) > 0) {
        char *local_path = cat_path(g_client_info.replicate_path, path);
        fd = libc_openat(AT_FDCWD, local_path, O_RDONLY | O_DIRECTORY);
        if(fd < 0) {
            return NULL;
        }
        fd = cfs_errno(cfs_openat_fd(g_client_info.cfs_client_id, AT_FDCWD, path, O_RDONLY | O_DIRECTORY, 0, fd));
    } else {
        fd = cfs_errno(cfs_openat(g_client_info.cfs_client_id, AT_FDCWD, path, O_RDONLY | O_DIRECTORY, 0));
    }
    free(path);

    if(fd < 0) {
        return NULL;
    }
    if(fd & CFS_FD_MASK) {
        cfs_close(g_client_info.cfs_client_id, fd);
        return NULL;
    }

    cfs_file_t cfs_file;
    cfs_get_file(g_client_info.cfs_client_id, fd, &cfs_file);
    if(record_open_file(&cfs_file) < 0) {
        fprintf(stderr, "cache opendir %d failed.\n", fd);
        fd = -1;
    }

    fd |= CFS_FD_MASK;
    size_t allocation = sizeof(struct dirent);
    DIR *dirp = (DIR *)malloc(sizeof(DIR) + allocation);
    if(dirp == NULL) {
        return NULL;
    }
    dirp->fd = fd;
    dirp->allocation = allocation;
    dirp->size = 0;
    dirp->offset = 0;
    dirp->filepos = 0;
    return dirp;
}

DIR *real_fdopendir(int fd) {
    bool is_cfs = fd_in_cfs(fd);
    if(!g_hook || !is_cfs) {
        return libc_fdopendir(fd);
    }

    size_t allocation = sizeof(struct dirent);
    DIR *dirp = (DIR *)malloc(sizeof(DIR) + allocation);
    if(dirp == NULL) {
        return NULL;
    }
    dirp->fd = fd;
    dirp->allocation = allocation;
    dirp->size = 0;
    dirp->offset = 0;
    dirp->filepos = 0;
    #ifdef _CFS_DEBUG
    log_debug("hook %s, fd:%d\n", __func__, fd & ~CFS_FD_MASK);
    #endif
    return dirp;
}

struct dirent *real_readdir(DIR *dirp) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    if(dirp == NULL) {
        errno = EBADF;
        return NULL;
    }
    bool is_cfs = fd_in_cfs(dirp->fd);
    if(!g_hook || !is_cfs) {
        return libc_readdir(dirp);
    }

    struct dirent *dp;
    if(dirp->offset >= dirp->size) {
        int fd = get_cfs_fd(dirp->fd);
        int count;
        count = cfs_getdents(g_client_info.cfs_client_id, fd, dirp->data, dirp->allocation);
        if(count <= 0) {
            if(count < 0) {
                errno = EBADF;
            }
            return NULL;
        }
        dirp->size = count;
        dirp->offset = 0;
    }

    dp = (struct dirent *) &dirp->data[dirp->offset];
    dirp->offset += dp->d_reclen;
    dirp->filepos = dp->d_off;
    return dp;
}

int real_readdir_r(DIR *dirp, struct dirent *entry, struct dirent **result) {
    if(dirp == NULL || entry == NULL || result == NULL) {
        errno = EBADF;
        return -1;
    }
    bool is_cfs = fd_in_cfs(dirp->fd);
    if(!g_hook || !is_cfs) {
        return libc_readdir_r(dirp, entry, result);
    }

    if(dirp->offset >= dirp->size) {
        int fd = get_cfs_fd(dirp->fd);
        int count;
        count = cfs_getdents(g_client_info.cfs_client_id, fd, dirp->data, dirp->allocation);
        if(count <= 0) {
            if(count < 0) {
                errno = EBADF;
                return -1;
            }
            *result = NULL;
            return 0;
        }
        dirp->size = count;
        dirp->offset = 0;
    }

    struct dirent *dp = (struct dirent *) &dirp->data[dirp->offset];
    *result = (struct dirent *)memcpy(entry, dp, dp->d_reclen);
    dirp->offset += dp->d_reclen;
    dirp->filepos = dp->d_off;
    return 0;
}

int real_closedir(DIR *dirp) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    if(dirp == NULL) {
        errno = EBADF;
        return -1;
    }

    int re;
    bool is_cfs = fd_in_cfs(dirp->fd);
    if(g_hook && is_cfs) {
        re = close_cfs_fd(dirp->fd);
        free(dirp);
    } else {
        re = libc_closedir(dirp);
    }
    return re;
}

static char *cfs_realpath(const char *cfs_path, char *resolved_path) {
    char *buf = NULL;
    int buf_len = 0;
    char *res_path = resolved_path;
    if(res_path == NULL) {
        res_path = (char *)malloc(PATH_MAX);
        memset(res_path, 0, PATH_MAX);
    }
    ssize_t res2 = cfs_errno_ssize_t(cfs_readlink(g_client_info.cfs_client_id, cfs_path, res_path, PATH_MAX));
    if(errno == EINVAL) {
        memcpy(res_path, cfs_path, strlen(cfs_path) + 1);
        errno = 0;
    }
    if(errno == 0) {
        if(res_path[0] == '/') {
            buf = cat_path(g_client_info.mount_point, &res_path[1]);
            buf_len = strlen(buf);
            if(buf_len >= PATH_MAX) {
                errno = ENAMETOOLONG;
            } else {
                errno = 0;
            }
        } else {
            errno = ENOENT;
        }
    }

    if(errno != 0) {
        if(buf != NULL) {
            free(buf);
        }
        if(resolved_path == NULL) {
            free(res_path);
        }
        res_path = NULL;
    } else {
        memcpy(res_path, buf, buf_len);
        res_path[buf_len] = 0;
        free(buf);
    }
    return res_path;
}

char *real_realpath_chk(const char *path, char *resolved_path, size_t resolvedlen) {
    char *re = NULL;
    char *cfs_path = get_cfs_path(path);
    if(g_hook && cfs_path != NULL) {
        re = cfs_realpath(cfs_path, resolved_path);
    } else {
        re = libc_realpath_chk(path, resolved_path, resolvedlen);
    }
    free(cfs_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs: %d, pathname:%s, re:%s\n", __func__, cfs_path != NULL, path, re);
    #endif
    return re;
}

char *real_realpath(const char *path, char *resolved_path) {
    char *re = NULL;
    char *cfs_path = get_cfs_path(path);
    if(g_hook && cfs_path != NULL) {
        re = cfs_realpath(cfs_path, resolved_path);
    } else {
        re = libc_realpath(path, resolved_path);
    }
    free(cfs_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs: %d, pathname:%s, re:%s\n", __func__, cfs_path != NULL, path, re);
    #endif
    return re;
}

/*
 * Link operations
 */

// link between CFS and ordinary file is not allowed
int real_linkat(int olddirfd, const char *old_pathname,
           int newdirfd, const char *new_pathname, int flags) {
    bool is_cfs_old = false;
    char *old_path = NULL;
    char *local_old_path = NULL;
    if((old_pathname != NULL && old_pathname[0] == '/') || olddirfd == AT_FDCWD) {
        old_path = get_cfs_path(old_pathname);
        local_old_path = cat_path(g_client_info.replicate_path, old_path);
        is_cfs_old = (old_path != NULL);
    } else {
        is_cfs_old = fd_in_cfs(olddirfd);
        if(is_cfs_old) {
            olddirfd = get_cfs_fd(olddirfd);
        }
    }

    bool is_cfs_new = false;
    char *new_path = NULL;
    char *local_new_path = NULL;
    if((new_pathname != NULL && new_pathname[0] == '/') || newdirfd == AT_FDCWD) {
        new_path = get_cfs_path(new_pathname);
        local_new_path = cat_path(g_client_info.replicate_path, new_path);
        is_cfs_new = (new_path != NULL);
    } else {
        is_cfs_new = fd_in_cfs(newdirfd);
        if(is_cfs_new) {
            newdirfd = get_cfs_fd(newdirfd);
        }
    }

    const char *cfs_old_path = (old_path == NULL) ? old_pathname : old_path;
    const char *cfs_new_path = (new_path == NULL) ? new_pathname : new_path;
    int re = -1;
    if(g_hook && is_cfs_old && is_cfs_new) {
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_linkat(olddirfd, local_old_path, newdirfd, local_new_path, flags);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_linkat(g_client_info.cfs_client_id, olddirfd, cfs_old_path, newdirfd, cfs_new_path, flags));
    } else if(!g_hook || (!is_cfs_old && !is_cfs_new)) {
        re = libc_linkat(olddirfd, old_pathname, newdirfd, new_pathname, flags);
    }

log:
    free(old_path);
    free(new_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, olddirfd:%d, old_pathname:%s, is_cfs_old:%d, newdirfd:%d, new_pathname:%s, is_cfs_new:%d, flags:%#x, re:%d\n", __func__, olddirfd, old_pathname, is_cfs_old, newdirfd, new_pathname, is_cfs_new, flags, re);
    #endif
    return re;
}

// symlink a CFS linkpath to ordinary file target is not allowed
int real_symlinkat(const char *target, int dirfd, const char *linkpath) {
    if(target[0] != '/') {
        errno = EINVAL;
        return -1;
    }
    char *cfs_target = get_cfs_path(target);
    char *local_target = NULL;
    bool is_cfs = false;
    char *cfs_linkpath = NULL;
    char *local_linkpath = NULL;
    if((linkpath != NULL && linkpath[0] == '/') || dirfd == AT_FDCWD) {
        cfs_linkpath = get_cfs_path(linkpath);
        local_linkpath = cat_path(g_client_info.replicate_path, cfs_linkpath);
        is_cfs = (cfs_linkpath != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    int re = -1;
    if(g_hook && is_cfs && cfs_target != NULL) {
        if(strlen(g_client_info.replicate_path) > 0) {
            local_target = cat_path(g_client_info.replicate_path, cfs_target);
            re = libc_symlinkat(local_target, dirfd, local_linkpath);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_symlinkat(g_client_info.cfs_client_id, cfs_target, dirfd, (cfs_linkpath == NULL) ? linkpath : cfs_linkpath));
    } else if(!g_hook || !is_cfs) {
        re = libc_symlinkat(target, dirfd, linkpath);
    }

log:
    free(cfs_target);
    free(cfs_linkpath);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, target:%s, dirfd:%d, linkpath:%s, re:%d\n", __func__, target, dirfd, linkpath, re);
    #endif
    return re;
}

int real_unlinkat(int dirfd, const char *pathname, int flags) {
    bool is_cfs = false;
    char *path = NULL;
    char *local_path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        local_path = cat_path(g_client_info.replicate_path, path);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    if(g_hook && is_cfs) {
        bool is_root = (path != NULL && (strlen(path) == 0 || (strlen(path) == 1 && path[0] == '/')));
        if(!is_root && strlen(g_client_info.replicate_path) > 0) {
            re = libc_unlinkat(dirfd, local_path != NULL ? local_path : pathname, flags);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_unlinkat(g_client_info.cfs_client_id, dirfd, cfs_path, flags));
    } else {
        re = libc_unlinkat(dirfd, pathname, flags);
    }

log:
    free(path);
    free(local_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, dirfd:%d, pathname:%s, flags:%#x, re:%d\n", __func__, is_cfs, dirfd, pathname, flags, re);
    #endif
    return re;
}

ssize_t real_readlinkat(int dirfd, const char *pathname, char *buf, size_t size) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    bool is_cfs = false;
    char *cfs_path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        cfs_path = get_cfs_path(pathname);
        is_cfs = (cfs_path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    ssize_t re;
    if(g_hook && is_cfs) {
        re = cfs_errno_ssize_t(cfs_readlinkat(g_client_info.cfs_client_id, dirfd, cfs_path, buf, size));
        if(re > 0 && buf[0] == '/') {
            char* tmp_path = cat_path(g_client_info.mount_point, &buf[1]);
            re = strlen(g_client_info.mount_point) + re;
            if(re > size) {
                re = size;
            }
            memcpy(buf, tmp_path, re);
            free(tmp_path);
        }
    } else {
        re = libc_readlinkat(dirfd, pathname, buf, size);
    }
    free(cfs_path);
    return re;
}


/*
 * Basic file attributes
 *
 * According to sys/stat.h, stat, fstat, lstat, fstatat are macros in glibc 2.17,
 * the actually called functions are __xstat, __fxstat, __lxstat, __fxstatat,
 * respectively. And because they are handled in header file, the original
 * functions cannot be intercepted.
 *
 * The 64-bit versions cannot be ignored, or realized as weak symbols, because
 * in glibc the original versions and the 64-bit versions have different signatures,
 * and struct stat and struct stat64 are defined independently.
 */

int real_stat(int ver, const char *pathname, struct stat *statbuf) {
    char *path = get_cfs_path(pathname);
    int re;
    if(g_hook && path != NULL) {
        re = cfs_errno(cfs_stat(g_client_info.cfs_client_id, path, statbuf));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            char *local_path = cat_path(g_client_info.replicate_path, path);
            struct stat statbuf_local;
            re = libc_stat(ver, local_path, &statbuf_local);
            free(local_path);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, path:%s, st_size:%d, st_size_local:%d\n", __func__, path != NULL, pathname, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_stat(ver, pathname, statbuf);
    }

log:
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, pathname:%s, re:%d\n", __func__, path != NULL, pathname, re);
    #endif
    return re;
}

int real_stat64(int ver, const char *pathname, struct stat64 *statbuf) {
    char *path = get_cfs_path(pathname);
    int re;
    if(g_hook && path != NULL) {
        re = cfs_errno(cfs_stat64(g_client_info.cfs_client_id, path, statbuf));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            char *local_path = cat_path(g_client_info.replicate_path, path);
            struct stat64 statbuf_local;
            re = libc_stat64(ver, local_path, &statbuf_local);
            free(local_path);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, path:%s, st_size:%d, st_size_local:%d\n", __func__, path != NULL, pathname, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_stat64(ver, pathname, statbuf);
    }

log:
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, pathname:%s, re:%d\n", __func__, path != NULL, pathname, re);
    #endif
    return re;
}

int real_lstat(int ver, const char *pathname, struct stat *statbuf) {
    char *path = get_cfs_path(pathname);
    int re;
    if(g_hook && path != NULL) {
        re = cfs_errno(cfs_lstat(g_client_info.cfs_client_id, path, statbuf));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            char *local_path = cat_path(g_client_info.replicate_path, path);
            struct stat statbuf_local;
            re = libc_lstat(ver, local_path, &statbuf_local);
            free(local_path);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, path:%s, st_size:%d, st_size_local:%d\n", __func__, path != NULL, pathname, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_lstat(ver, pathname, statbuf);
    }

log:
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, pathname:%s, re:%d\n", __func__, path != NULL, pathname, re);
    #endif
    return re;
}

int real_lstat64(int ver, const char *pathname, struct stat64 *statbuf) {
    char *path = get_cfs_path(pathname);
    int re;
    if(g_hook && path != NULL) {
        re = cfs_errno(cfs_lstat64(g_client_info.cfs_client_id, path, statbuf));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            char *local_path = cat_path(g_client_info.replicate_path, path);
            struct stat64 statbuf_local;
            re = libc_lstat64(ver, local_path, &statbuf_local);
            free(local_path);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, path:%s, st_size:%d, st_size_local:%d\n", __func__, path != NULL, pathname, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_lstat64(ver, pathname, statbuf);
    }

log:
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, pathname:%s, re:%d\n", __func__, path != NULL, pathname, re);
    #endif
    return re;
}

int real_fstat(int ver, int fd, struct stat *statbuf) {
    int re;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        re = cfs_errno(cfs_fstat(g_client_info.cfs_client_id, fd, statbuf));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            struct stat statbuf_local;
            re = libc_fstat(ver, fd, &statbuf_local);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, fd:%d, st_size:%d, st_size_local:%d\n", __func__, is_cfs, fd, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_fstat(ver, fd, statbuf);
    }

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, fd:%d, re:%d\n", __func__, is_cfs, fd, re);
    #endif
    return re;
}

int real_fstat64(int ver, int fd, struct stat64 *statbuf) {
    int re;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        re = cfs_errno(cfs_fstat64(g_client_info.cfs_client_id, fd, statbuf));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            struct stat64 statbuf_local;
            re = libc_fstat64(ver, fd, &statbuf_local);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, fd:%d, st_size:%d, st_size_local:%d\n", __func__, is_cfs, fd, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_fstat64(ver, fd, statbuf);
    }

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, fd:%d, re:%d\n", __func__, is_cfs, fd, re);
    #endif
    return re;
}

int real_fstatat(int ver, int dirfd, const char *pathname, struct stat *statbuf, int flags) {
    bool is_cfs = false;
    char *path = NULL;
    char *local_path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        local_path = cat_path(g_client_info.replicate_path, path);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    if(g_hook && is_cfs) {
        re = cfs_errno(cfs_fstatat(g_client_info.cfs_client_id, dirfd, cfs_path, statbuf, flags));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            struct stat statbuf_local;
            re = libc_fstatat(ver, dirfd, local_path != NULL ? local_path : pathname, &statbuf_local, flags);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, dirfd:%d, path:%s, st_size:%d, st_size_local:%d\n", __func__, is_cfs, dirfd, pathname, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_fstatat(ver, dirfd, pathname, statbuf, flags);
    }

log:
    free(path);
    free(local_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, dirfd:%d, pathname:%s, re:%d\n", __func__, is_cfs, dirfd, pathname, re);
    #endif
    return re;
}

int real_fstatat64(int ver, int dirfd, const char *pathname, struct stat64 *statbuf, int flags) {
    bool is_cfs = false;
    char *path = NULL;
    char *local_path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        local_path = cat_path(g_client_info.replicate_path, path);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    if(g_hook && is_cfs) {
        re = cfs_errno(cfs_fstatat64(g_client_info.cfs_client_id, dirfd, cfs_path, statbuf, flags));
        if(re == 0) {
            inode_info_t *inode_info = get_open_inode(statbuf->st_ino);
            if(inode_info != NULL) {
                statbuf->st_size = inode_info->size;
            }
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            struct stat64 statbuf_local;
            re = libc_fstatat64(ver, dirfd, local_path != NULL ? local_path : pathname, &statbuf_local, flags);
            free(local_path);
            if(re < 0) {
                goto log;
            }
            if(statbuf->st_mode & S_IFREG && statbuf->st_size != statbuf_local.st_size) {
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, dirfd:%d, path:%s, st_size:%d, st_size_local:%d\n", __func__, is_cfs, dirfd, pathname, statbuf->st_size, statbuf_local.st_size);
            }
        }
    } else {
        re = libc_fstatat64(ver, dirfd, pathname, statbuf, flags);
    }

log:
    free(path);
    free(local_path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, dirfd:%d, pathname:%s, re:%d\n", __func__, is_cfs, dirfd, pathname, re);
    #endif
    return re;
}

#ifdef HAVE_STATX
int real_statx(int dirfd, const char *pathname, int flags, unsigned int mask, struct statx *statxbuf) {
    bool is_cfs = false;
    char *path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs) {
            dirfd = get_cfs_fd(dirfd);
        }
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    if(g_hook && path != NULL) {
        struct stat statbuf;
        re = cfs_errno(cfs_fstatat(g_client_info.cfs_client_id, dirfd, cfs_path, &statbuf, flags));
        if(re == 0) {
            statxbuf->stx_mask = 0;
            statxbuf->stx_attributes = 0;
            statxbuf->stx_attributes_mask = 0;
            statxbuf->stx_rdev_major = 0;
            statxbuf->stx_rdev_minor = 0;
            statxbuf->stx_dev_major = 0;
            statxbuf->stx_dev_minor = 0;
            statxbuf->stx_ino = statbuf.st_ino;
            statxbuf->stx_size = statbuf.st_size;
            statxbuf->stx_nlink = statbuf.st_nlink;
            statxbuf->stx_blksize = statbuf.st_blksize;
            statxbuf->stx_uid = statbuf.st_uid;
            statxbuf->stx_gid = statbuf.st_gid;
            statxbuf->stx_blocks = statbuf.st_blocks;
            statxbuf->stx_mode = statbuf.st_mode;
            statxbuf->stx_atime.tv_sec = statbuf.st_atim.tv_sec;
            statxbuf->stx_atime.tv_nsec = statbuf.st_atim.tv_nsec;
            statxbuf->stx_btime.tv_sec = statbuf.st_ctim.tv_sec;
            statxbuf->stx_btime.tv_nsec = statbuf.st_ctim.tv_nsec;
            statxbuf->stx_mtime.tv_sec = statbuf.st_mtim.tv_sec;
            statxbuf->stx_mtime.tv_nsec = statbuf.st_mtim.tv_nsec;
            statxbuf->stx_ctime.tv_sec = statbuf.st_ctim.tv_sec;
            statxbuf->stx_ctime.tv_nsec = statbuf.st_ctim.tv_nsec;
            pthread_rwlock_rdlock(&g_client_info.open_inodes_lock);
            auto it = g_client_info.open_inodes.find(statxbuf->stx_ino);
            if(it != g_client_info.open_inodes.end()) {
                statxbuf->stx_size = it->second->size;
            }
            pthread_rwlock_unlock(&g_client_info.open_inodes_lock);
        }
    } else {
        re = libc_statx(dirfd, pathname, flags, mask, statxbuf);
    }
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, dirfd:%d, pathname:%s, flags:%x, mask:%x, re:%d\n", __func__, is_cfs, dirfd, pathname, flags, mask, re);
    #endif
    return re;
}
#endif

int real_fchmod(int fd, mode_t mode) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    return fd_in_cfs(fd) ? cfs_errno(cfs_fchmod(g_client_info.cfs_client_id, get_cfs_fd(fd), mode)) : libc_fchmod(fd, mode);
}

int real_fchmodat(int dirfd, const char *pathname, mode_t mode, int flags) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    bool is_cfs = false;
    char *path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    re = g_hook && is_cfs ? cfs_errno(cfs_fchmodat(g_client_info.cfs_client_id, dirfd, cfs_path, mode, flags)) :
        libc_fchmodat(dirfd, pathname, mode, flags);
    free(path);
    return re;
}

int real_lchown(const char *pathname, uid_t owner, gid_t group) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno(cfs_lchown(g_client_info.cfs_client_id, path, owner, group)) :
             libc_lchown(pathname, owner, group);
    free(path);
    return re;
}

int real_fchown(int fd, uid_t owner, gid_t group) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    return g_hook && fd_in_cfs(fd) ? cfs_errno(cfs_fchown(g_client_info.cfs_client_id, get_cfs_fd(fd), owner, group)) :
        libc_fchown(fd, owner, group);
}

int real_fchownat(int dirfd, const char *pathname, uid_t owner, gid_t group, int flags) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    bool is_cfs = false;
    char *path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    re = g_hook && is_cfs ? cfs_errno(cfs_fchownat(g_client_info.cfs_client_id, dirfd, cfs_path, owner, group, flags)):
        libc_fchownat(dirfd, pathname, owner, group, flags);
    free(path);
    return re;
}

int real_utime(const char *pathname, const struct utimbuf *times) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    struct timespec *pts = NULL;
    struct timespec ts[2];
    if(times != NULL) {
        ts[0].tv_sec = times->actime;
        ts[0].tv_nsec = 0;
        ts[1].tv_sec = times->modtime;
        ts[1].tv_nsec = 0;
        pts = &ts[0];
    }
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno(cfs_utimens(g_client_info.cfs_client_id, path, pts, 0)) :
            libc_utime(pathname, times);
    free(path);
    return re;
}

int real_utimes(const char *pathname, const struct timeval *times) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    struct timespec *pts = NULL;
    struct timespec ts[2];
    if(times != NULL) {
        ts[0].tv_sec = times[0].tv_sec;
        ts[0].tv_nsec = times[0].tv_usec*1000;
        ts[1].tv_sec = times[1].tv_sec;
        ts[1].tv_nsec = times[1].tv_usec*1000;
        pts = &ts[0];
    }
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno(cfs_utimens(g_client_info.cfs_client_id, path, pts, 0)) :
            libc_utimes(pathname, times);
    free(path);
    return re;
}

int real_futimesat(int dirfd, const char *pathname, const struct timeval times[2]) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    bool is_cfs = false;
    char *path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    struct timespec *pts = NULL;
    struct timespec ts[2];
    if(times != NULL) {
        ts[0].tv_sec = times[0].tv_sec;
        ts[0].tv_nsec = times[0].tv_usec*1000;
        ts[1].tv_sec = times[1].tv_sec;
        ts[1].tv_nsec = times[1].tv_usec*1000;
        pts = &ts[0];
    }
    int re;
    re = g_hook && is_cfs ? cfs_errno(cfs_utimensat(g_client_info.cfs_client_id, dirfd, cfs_path, pts, 0)) :
        libc_futimesat(dirfd, pathname, times);
    free(path);
    return re;
}

int real_utimensat(int dirfd, const char *pathname, const struct timespec times[2], int flags) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    bool is_cfs = false;
    char *path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    re = g_hook && is_cfs ? cfs_errno(cfs_utimensat(g_client_info.cfs_client_id, dirfd, cfs_path, times, flags)) :
        libc_utimensat(dirfd, pathname, times, flags);
    free(path);
    return re;
}

int real_futimens(int fd, const struct timespec times[2]) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    return g_hook && fd_in_cfs(fd) ? cfs_errno(cfs_futimens(g_client_info.cfs_client_id, get_cfs_fd(fd), times)) : libc_futimens(fd, times);
}

int real_faccessat(int dirfd, const char *pathname, int mode, int flags) {
    bool is_cfs = false;
    char *path = NULL;
    char *local_path = NULL;
    if((pathname != NULL && pathname[0] == '/') || dirfd == AT_FDCWD) {
        path = get_cfs_path(pathname);
        local_path = cat_path(g_client_info.replicate_path, path);
        is_cfs = (path != NULL);
    } else {
        is_cfs = fd_in_cfs(dirfd);
        if(is_cfs)
            dirfd = get_cfs_fd(dirfd);
    }

    const char *cfs_path = (path == NULL) ? pathname : path;
    int re;
    if(g_hook && is_cfs) {
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_faccessat(dirfd, local_path != NULL ? local_path : pathname, mode, flags);
            if(re < 0) {
                goto log;
            }
        }
        re = cfs_errno(cfs_faccessat(g_client_info.cfs_client_id, dirfd, cfs_path, mode, flags));
    } else {
        re = libc_faccessat(dirfd, pathname, mode, flags);
    }

log:
    free(path);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, dirfd:%d, pathname:%s, mode:%d, flags:%#x, re:%d\n",
    __func__, is_cfs, dirfd, pathname, mode, flags, re);
    #endif
    return re;
}


/*
 * Extended file attributes
 */

int real_setxattr(const char *pathname, const char *name,
        const void *value, size_t size, int flags) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno(cfs_setxattr(g_client_info.cfs_client_id, path, name, value, size, flags)) :
             libc_setxattr(pathname, name, value, size, flags);
    free(path);
    return re;
}

int real_lsetxattr(const char *pathname, const char *name,
             const void *value, size_t size, int flags) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno(cfs_lsetxattr(g_client_info.cfs_client_id, path, name, value, size, flags)) :
             libc_lsetxattr(pathname, name, value, size, flags);
    free(path);
    return re;
}

int real_fsetxattr(int fd, const char *name, const void *value, size_t size, int flags) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    return g_hook && fd_in_cfs(fd) ? cfs_errno(cfs_fsetxattr(g_client_info.cfs_client_id, get_cfs_fd(fd), name, value, size, flags)) :
           libc_fsetxattr(fd, name, value, size, flags);
}

ssize_t real_getxattr(const char *pathname, const char *name, void *value, size_t size) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno_ssize_t(cfs_getxattr(g_client_info.cfs_client_id, path, name, value, size)) :
             libc_getxattr(pathname, name, value, size);
    free(path);
    return re;
}

ssize_t real_lgetxattr(const char *pathname, const char *name, void *value, size_t size) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno_ssize_t(cfs_lgetxattr(g_client_info.cfs_client_id, path, name, value, size)) :
             libc_lgetxattr(pathname, name, value, size);
    free(path);
    return re;
}

ssize_t real_fgetxattr(int fd, const char *name, void *value, size_t size) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    return g_hook && fd_in_cfs(fd) ? cfs_errno_ssize_t(cfs_fgetxattr(g_client_info.cfs_client_id, get_cfs_fd(fd), name, value, size)) :
           libc_fgetxattr(fd, name, value, size);
}

ssize_t real_listxattr(const char *pathname, char *list, size_t size) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno_ssize_t(cfs_listxattr(g_client_info.cfs_client_id, path, list, size)) :
             libc_listxattr(pathname, list, size);
    free(path);
    return re;
}

ssize_t real_llistxattr(const char *pathname, char *list, size_t size) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    ssize_t re;
    re = (g_hook && path != NULL) ? cfs_errno_ssize_t(cfs_llistxattr(g_client_info.cfs_client_id, path, list, size)) :
             libc_llistxattr(pathname, list, size);
    free(path);
    return re;
}

ssize_t real_flistxattr(int fd, char *list, size_t size) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    return g_hook && fd_in_cfs(fd) ? cfs_errno_ssize_t(cfs_flistxattr(g_client_info.cfs_client_id, get_cfs_fd(fd), list, size)) :
           libc_flistxattr(fd, list, size);
}

int real_removexattr(const char *pathname, const char *name) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno(cfs_removexattr(g_client_info.cfs_client_id, path, name)) :
             libc_removexattr(pathname, name);
    free(path);
    return re;
}

int real_lremovexattr(const char *pathname, const char *name) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    char *path = get_cfs_path(pathname);
    int re;
    re = (g_hook && path != NULL) ? cfs_errno(cfs_lremovexattr(g_client_info.cfs_client_id, path, name)) :
             libc_lremovexattr(pathname, name);
    free(path);
    return re;
}

int real_fremovexattr(int fd, const char *name) {
    #ifdef _CFS_DEBUG
    log_debug("hook %s\n", __func__);
    #endif
    return g_hook && fd_in_cfs(fd) ? cfs_errno(cfs_fremovexattr(g_client_info.cfs_client_id, get_cfs_fd(fd), name)) :
           libc_fremovexattr(fd, name);
}


/*
 * File descriptor manipulations
 */

int real_fcntl(int fd, int cmd, ...) {
    va_list args;
    va_start(args, cmd);
    void *arg = va_arg(args, void *);
    va_end(args);

    int re, newfd = 0;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_fcntl(fd, cmd, arg);
            if(re < 0) {
                goto log;
            }
            if(cmd == F_SETLK || cmd == F_SETLKW) {
                re = cfs_fcntl_lock(g_client_info.cfs_client_id, fd, cmd, (struct flock *)arg);
            } else if(cmd == F_DUPFD || cmd == F_DUPFD_CLOEXEC) {
                newfd = re;
                re = dup_fd(fd, newfd);
                if(re != newfd) {
                    goto log;
                }
            } else {
                re = cfs_fcntl(g_client_info.cfs_client_id, fd, cmd, (intptr_t)arg);
            }
        } else {
            if(cmd == F_SETLK || cmd == F_SETLKW) {
                re = cfs_fcntl_lock(g_client_info.cfs_client_id, fd, cmd, (struct flock *)arg);
            } else if(cmd == F_DUPFD || cmd == F_DUPFD_CLOEXEC) {
                int new_fd = gen_fd((long)arg);
                re = dup_fd(fd, new_fd);
            } else {
                re = cfs_fcntl(g_client_info.cfs_client_id, fd, cmd, (intptr_t)arg);
            }
        }
        re = cfs_errno(re);
    } else {
        re = libc_fcntl(fd, cmd, arg);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    map<int, string> cmd_str = {{F_DUPFD, "F_DUPFD"}, {F_DUPFD_CLOEXEC, "F_DUPFD_CLOEXEC"}, {F_GETFD, "F_GETFD"}, {F_SETFD, "F_SETFD"}, {F_GETFL, "F_GETFL"}, {F_SETFL, "F_SETFL"}, {F_SETLK, "F_SETLK"}, {F_GETLK, "F_GETLK"}};
    auto it = cmd_str.find(cmd);
    log_debug("hook %s, is_cfs:%d, fd:%d, cmd:%d(%s), arg:%u(%s), re:%d, newfd:%d\n", __func__, is_cfs, fd, cmd,
    it != cmd_str.end() ? it->second.c_str() : "", (intptr_t)arg, (cmd==F_SETFL&&(intptr_t)arg&O_DIRECT)?"O_DIRECT":"", re, newfd);
    #endif
    return re;
}

int close_fd(int fd) {
    if(fd_in_cfs(fd)) {
        return close_cfs_fd(fd);
    }
    return libc_close(fd);
}

int real_dup(int oldfd) {
    bool is_cfs = fd_in_cfs(oldfd);
    int re = -1;
    if (oldfd < 0) {
        goto log;
    }

    if(g_hook && is_cfs) {
        oldfd = get_cfs_fd(oldfd);
        re = cfs_errno(cfs_alloc_fd(g_client_info.cfs_client_id));
        if(re > 0) {
            // the dup fd should be distinct from any libc fd
            int newfd = re | CFS_FD_MASK;
            re = dup_fd(oldfd, newfd);
        }
    } else {
        re = libc_dup(oldfd);
    }

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, oldfd:%d, re:%d\n", __func__, is_cfs, oldfd, re);
    #endif
    return re;
}

int real_dup2(int oldfd, int newfd) {
    bool is_cfs = fd_in_cfs(oldfd);
    int re = newfd;
    if (newfd == oldfd || newfd < 0)
        goto log;

    // If newfd was open, close it before being reused
    re = close_fd(newfd);

    if(g_hook && is_cfs) {
        oldfd = get_cfs_fd(oldfd);
        re = dup_fd(oldfd, newfd);
    } else {
        re = libc_dup2(oldfd, newfd);
    }

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, oldfd:%d, newfd:%d, re:%d\n", __func__, is_cfs, oldfd, newfd, re);
    #endif
    return re;
}

int real_dup3(int oldfd, int newfd, int flags) {
    bool is_cfs = fd_in_cfs(oldfd);
    int re = newfd;
    if (newfd == oldfd || newfd < 0)
        goto log;

    // If newfd was open, close it before being reused
    re = close_fd(newfd);

    if(g_hook && is_cfs) {
        oldfd = get_cfs_fd(oldfd);
        re = dup_fd(oldfd, newfd);
    } else {
        re = libc_dup3(oldfd, newfd, flags);
    }

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, oldfd:%d, newfd:%d, flags:%#x, re:%d\n", __func__, is_cfs, oldfd, newfd, flags, re);
    #endif
    return re;
}

/*
 * Read & Write
 */

static ssize_t cfs_pread_sock(int64_t id, int fd, void *buf, size_t count, off_t offset, bool hasRefreshed) {
    int max_count = 3;
    cfs_read_req_t *req = (cfs_read_req_t *)calloc(max_count, sizeof(cfs_read_req_t));
	int req_count = cfs_read_requests(id, fd, buf, count, offset, req, max_count);
    ssize_t read = 0;
    bool has_err = req_count < 0;
    for(int i = 0; i < req_count; i++) {
        if(req[i].size == 0) {
            break;
        }
        if(req[i].partition_id == 0) {
            memset((char *)buf + read, 0, req[i].size);
            read += req[i].size;
            continue;
        }
        packet_t *p = new_read_packet(req[i].partition_id, req[i].extent_id, req[i].extent_offset, (char *)buf + read, req[i].size, req[i].file_offset);
        if(p == NULL) {
            has_err = true;
            break;
        }
        int sock_fd = get_conn(g_client_info.conn_pool, req[i].dp_host, req[i].dp_port);
        if(sock_fd < 0) {
            free(p);
            has_err = true;
            break;
        }
        ssize_t re = write_sock(sock_fd, p);
        if(re < 0) {
            free(p);
            close(sock_fd);
            has_err = true;
            break;
        }
        re = get_read_reply(sock_fd, p);
        free(p);
        if(re < 0) {
            close(sock_fd);
            has_err = true;
            break;
        }
        #ifdef _CFS_DEBUG
        log_debug("cfs_pread_sock read sock, file_offset:%d, host:%s, sock_fd:%d, dp:%d, extent:%d, extent_offset:%ld, size:%d, re:%d\n", req[i].file_offset, req[i].dp_host, sock_fd, req[i].partition_id, req[i].extent_id, req[i].extent_offset, req[i].size, re);
        #endif
        put_conn(g_client_info.conn_pool, req[i].dp_host, req[i].dp_port, sock_fd);
        read += re;
        if(re != req[i].size) {
            break;
        }
    }
    free(req);
    #ifdef _CFS_DEBUG
    log_debug("cfs_pread_sock, fd:%d, count:%d, offset:%ld, req_count:%d, read:%d, has_err:%d\n", fd, count, offset, req_count, read, has_err);
    #endif
    if((read < count && !hasRefreshed) || has_err) {
        read = cfs_pread(id, fd, buf, count, offset);
    }
    return read;
}

ssize_t real_read(int fd, void *buf, size_t count) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);

    off_t offset = 0;
    size_t size = 0;
    ssize_t re = -1, re_local = 0, re_cache = 0;

    bool is_cfs = fd_in_cfs(fd);
    char *buf_local = NULL;
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        file_t *f = get_open_file(fd);
        if(f == NULL) {
            goto log;
        }
        offset = f->pos;
        if(strlen(g_client_info.replicate_path) > 0) {
            buf_local = (char *)malloc(count);
            if(buf_local == NULL) {
                re = -1;
                goto log;
            }
            if(libc_lseek(fd, offset, SEEK_SET) < 0) {
                goto log;
            }
            re_local = libc_read(fd, buf_local, count);
        }
        bool hasRefreshed = false;
        size = f->inode_info->size;
        re_cache = read_cache(f->inode_info, offset, count, buf);
        // If the read range exceed the current file size, refresh ek in case of the file being appended by other clients.
        // For binlog, the read thread of mysql may always read 8k, whether can read enough data or not.
        // So, binlog is an exception for refreshing ek.
        if(re_cache < count && offset + count >= size && f->file_type != FILE_TYPE_BIN_LOG) {
            flush_inode(f->inode_info);
            cfs_flush(g_client_info.cfs_client_id, fd);
            size = cfs_refresh_eks(g_client_info.cfs_client_id, f->inode_info->inode);
            if(size >= 0) {
                // whether the file size change or not, refresh eks is successlful;
                hasRefreshed = true;
            }
            update_inode_size(f->inode_info, size);
        }
        if(re_cache < count && offset + re_cache < size) {
            // data may reside both in cache and CFS, flush to prevent inconsistent read
            flush_inode_range(f->inode_info, offset, count);
            cfs_flush(g_client_info.cfs_client_id, fd);
            size_t new_count = count;
            if(offset + count > size) {
                new_count = size - offset;
            }
            re = cfs_errno_ssize_t(cfs_pread_sock(g_client_info.cfs_client_id, fd, buf, new_count, offset, hasRefreshed));
        } else {
            re = re_cache;
        }
        if(re > 0) {
            f->pos += re;
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            // Reading from local and CFS may be concurrent with writing to local and CFS.
            // There are two conditions in which data read from local and CFS may be different.
            // 1. read local -> write local -> write CFS -> read CFS
            // 2. write local -> read local -> read CFS -> write CFS
            // In contition 2, write CFS may be concurrent with read CFS, resulting in last bytes read being zero.
            ssize_t min_res = re_local < re ? re_local: re;
            if(re_local > 0 && re > 0 && memcmp(buf, buf_local, min_res)) {
                const char *fd_path = get_fd_path(fd);
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, fd:%d, path:%s, count:%d, offset:%ld, re_cfs:%d, re_local:%d\n", __func__, is_cfs, fd, fd_path, count, offset, re, re_local);
                find_diff_data(buf, buf_local, offset, min_res);
                cfs_flush_log();
                exit(1);
            }
            free(buf_local);
        }
    } else {
        #ifdef _CFS_DEBUG
        offset = lseek(fd, 0, SEEK_CUR);
        #endif
        re = libc_read(fd, buf, count);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    clock_gettime(CLOCK_REALTIME, &stop);
    long time = (stop.tv_sec - start.tv_sec)*1000000000 + stop.tv_nsec - start.tv_nsec;
    log_debug("hook %s, is_cfs:%d, fd:%d, path: %s, count:%d, offset:%ld, size:%d, re:%d, re_cache:%d, time:%d\n", __func__, is_cfs, fd, fd_path, count, offset, size, re, re_cache, time/1000);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_READ, start.tv_sec, start.tv_nsec);
    }
    return re;
}

ssize_t real_readv(int fd, const struct iovec *iov, int iovcnt) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    ssize_t re = -1;
    off_t offset;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        offset = f->pos;
        re = cfs_errno_ssize_t(cfs_preadv(g_client_info.cfs_client_id, fd, iov, iovcnt, f->pos));
        if(re > 0) {
            f->pos += re;
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            if(re <= 0) {
                goto log;
            }
            struct iovec iov_local[iovcnt];
            for(int i = 0; i < iovcnt; i++) {
                iov_local[i].iov_base = malloc(iov[i].iov_len);
                iov_local[i].iov_len = iov[i].iov_len;
            }
            if(libc_lseek(fd, offset, SEEK_SET) < 0) {
                goto log;
            }
            re = libc_readv(fd, iov_local, iovcnt);
            if(re <= 0) {
                goto log;
            }
            for(int i = 0; i < iovcnt; i++) {
                if(memcmp(iov[i].iov_base, iov_local[i].iov_base, iov[i].iov_len)) {
                    const char *fd_path = get_fd_path(fd);
                    log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, fd:%d, path:%s, offset:%ld, iovcnt:%d, iov_idx:%d, iov_len:%d\n", __func__, is_cfs, fd, fd_path, offset, iovcnt, i, iov[i].iov_len);
                    find_diff_data(iov[i].iov_base, iov_local[i].iov_base, offset, iov[i].iov_len);
                    cfs_flush_log();
                    exit(1);
                }
                free(iov_local[i].iov_base);
            }
        }
    } else {
        re = libc_readv(fd, iov, iovcnt);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, iovcnt:%d, re:%d\n", __func__, is_cfs, fd, fd_path, iovcnt, re);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_READ, start.tv_sec, start.tv_nsec);
    }
    return re;
}

ssize_t real_pread(int fd, void *buf, size_t count, off_t offset) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    ssize_t re = -1, re_local = 0, re_cache = 0;

    bool is_cfs = fd_in_cfs(fd);
    char *buf_local = NULL;
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            buf_local = (char *)malloc(count);
            if(buf_local == NULL) {
                re = -1;
                goto log;
            }
            re_local = libc_pread(fd, buf_local, count, offset);
        }
        file_t *f = get_open_file(fd);
        if(f == NULL) {
            goto log;
        }
        bool hasRefreshed = false;
        size_t size = f->inode_info->size;
        re_cache = read_cache(f->inode_info, offset, count, buf);
        if(re_cache < count && offset + count >= size && f->file_type != FILE_TYPE_BIN_LOG) {
            flush_inode(f->inode_info);
            cfs_flush(g_client_info.cfs_client_id, fd);
            size = cfs_refresh_eks(g_client_info.cfs_client_id, f->inode_info->inode);
            if(size >= 0) {
                // whether the file size change or not, refresh eks is successlful;
                hasRefreshed = true;
            }
            update_inode_size(f->inode_info, size);
        }
        if(re_cache < count && offset + re_cache < size) {
            // data may reside both in cache and CFS, flush to prevent inconsistent read
            flush_inode_range(f->inode_info, offset, count);
            cfs_flush(g_client_info.cfs_client_id, fd);
            size_t new_count = count;
            if(offset + count > size) {
                new_count = size - offset;
            }
            re = cfs_errno_ssize_t(cfs_pread_sock(g_client_info.cfs_client_id, fd, buf, new_count, offset, hasRefreshed));
        } else {
            re = re_cache;
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            ssize_t min_res = re_local < re ? re_local: re;
            if(re_local > 0 && re > 0 && memcmp(buf, buf_local, min_res)) {
                const char *fd_path = get_fd_path(fd);
                log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, fd:%d, path:%s, count:%d, offset:%ld, re_cfs:%d, re_local:%d\n", __func__, is_cfs, fd, fd_path, count, offset, re, re_local);
                find_diff_data(buf, buf_local, offset, min_res);
                cfs_flush_log();
                exit(1);
            }
            free(buf_local);
        }
    } else {
        re = libc_pread(fd, buf, count, offset);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, count:%d, offset:%ld, re:%d\n", __func__, is_cfs, fd, fd_path, count, offset, re);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_READ, start.tv_sec, start.tv_nsec);
    }
    return re;
}

ssize_t real_preadv(int fd, const struct iovec *iov, int iovcnt, off_t offset) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    ssize_t re = -1;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        re = cfs_errno_ssize_t(cfs_preadv(g_client_info.cfs_client_id, fd, iov, iovcnt, offset));
        if(strlen(g_client_info.replicate_path) > 0) {
            if(re <= 0) {
                goto log;
            }
            struct iovec iov_local[iovcnt];
            for(int i = 0; i < iovcnt; i++) {
                iov_local[i].iov_base = malloc(iov[i].iov_len);
                iov_local[i].iov_len = iov[i].iov_len;
            }
            re = libc_preadv(fd, iov_local, iovcnt, offset);
            if(re <= 0) {
                goto log;
            }
            for(int i = 0; i < iovcnt; i++) {
                if(memcmp(iov[i].iov_base, iov_local[i].iov_base, iov[i].iov_len)) {
                    const char *fd_path = get_fd_path(fd);
                    log_debug("hook %s, data from CFS and local is not consistent. is_cfs:%d, fd:%d, path:%s, iovcnt:%d, offset:%ld, iov_idx: %d\n", __func__, is_cfs, fd, fd_path, iovcnt, offset, i);
                    find_diff_data(iov[i].iov_base, iov_local[i].iov_base, offset, iov[i].iov_len);
                    cfs_flush_log();
                    exit(1);
                }
                free(iov_local[i].iov_base);
            }
        }
    } else {
        re = libc_preadv(fd, iov, iovcnt, offset);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, iovcnt:%d, offset:%ld, re:%d\n", __func__, is_cfs, fd, fd_path, iovcnt, offset, re);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_READ, start.tv_sec, start.tv_nsec);
    }
    return re;
}

ssize_t real_write(int fd, const void *buf, size_t count) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    off_t offset = 0;
    size_t size = 0;
    ssize_t re = -1, re_cache = 0, re_local = 0;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        if(f->flags&O_APPEND) {
            f->pos = f->inode_info->size;
        }
        offset = f->pos;
        re_cache = write_cache(f->inode_info, f->pos, count, buf);
        if(f->inode_info->cache_flag&FILE_CACHE_WRITE_THROUGH || re_cache < count) {
            //if(re_cache < count) log_debug("write cache fail, fd:%d, inode:%d, file_type:%d, offset:%ld, count:%d, size:%d, re_cache:%d\n", fd, f->inode, f->file_type, offset, count, f->size, re_cache);
            if(re_cache < count) {
                clear_inode_range(f->inode_info, f->pos, count);
            }
            re = cfs_errno_ssize_t(cfs_pwrite(g_client_info.cfs_client_id, fd, buf, count, f->pos));
        } else {
            re = re_cache;
        }
        if(re > 0) {
            f->pos += re;
            size = update_inode_size(f->inode_info, f->pos);
        } else {
            goto log;
        }
        if(strlen(g_client_info.replicate_path) > 0) {
            re_local = libc_write(fd, buf, count);
            if(re_local != re) {
                re = re_local;
                goto log;
            }
        }
    } else {
        #ifdef _CFS_DEBUG
        offset = lseek(fd, 0, SEEK_CUR);
        #endif
        re = libc_write(fd, buf, count);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    clock_gettime(CLOCK_REALTIME, &stop);
    long time = (stop.tv_sec - start.tv_sec)*1000000000 + stop.tv_nsec - start.tv_nsec;
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, count:%d, offset:%ld, size:%d, re:%d, re_cache:%d, re_local:%d time:%d\n", __func__, is_cfs, fd, fd_path, count, offset, size, re, re_cache, re_local, time/1000);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_WRITE, start.tv_sec, start.tv_nsec);
    }
    return re;
}

ssize_t real_writev(int fd, const struct iovec *iov, int iovcnt) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    ssize_t re = -1;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_writev(fd, iov, iovcnt);
            if(re < 0) {
                goto log;
            }
        }
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        if(f->flags&O_APPEND) {
            f->pos = f->inode_info->size;
        }
        re = cfs_errno_ssize_t(cfs_pwritev(g_client_info.cfs_client_id, fd, iov, iovcnt, f->pos));
        if(re > 0) {
            f->pos += re;
            update_inode_size(f->inode_info, f->pos);
        }
    } else {
        re = libc_writev(fd, iov, iovcnt);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, iovcnt:%d, re:%d\n", __func__, is_cfs, fd, fd_path, iovcnt, re);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_WRITE, start.tv_sec, start.tv_nsec);
    }
    return re;
}

ssize_t real_pwrite(int fd, const void *buf, size_t count, off_t offset) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    ssize_t re = -1, re_cache = 0;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_pwrite(fd, buf, count, offset);
            if(re < 0) {
                goto log;
            }
        }
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        re_cache = write_cache(f->inode_info, offset, count, buf);
        if(f->inode_info->cache_flag&FILE_CACHE_WRITE_THROUGH || re_cache < count) {
            if(re_cache < count) {
                clear_inode_range(f->inode_info, offset, count);
            }
            re = cfs_errno_ssize_t(cfs_pwrite(g_client_info.cfs_client_id, fd, buf, count, offset));
        } else {
            re = re_cache;
        }
        if(re > 0) {
            update_inode_size(f->inode_info, offset + re);
        }
    } else {
        re = libc_pwrite(fd, buf, count, offset);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    clock_gettime(CLOCK_REALTIME, &stop);
    long time = (stop.tv_sec - start.tv_sec)*1000000000 + stop.tv_nsec - start.tv_nsec;
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, count:%d, offset:%ld, re:%d, re_cache:%d, time:%d\n", __func__, is_cfs, fd, fd_path, count, offset, re, re_cache, time/1000);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_WRITE, start.tv_sec, start.tv_nsec);
    }
    return re;
}

ssize_t real_pwritev(int fd, const struct iovec *iov, int iovcnt, off_t offset) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    ssize_t re = -1;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_pwritev(fd, iov, iovcnt, offset);
            if(re < 0) {
                goto log;
            }
        }
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        re = cfs_errno_ssize_t(cfs_pwritev(g_client_info.cfs_client_id, fd, iov, iovcnt, offset));
        if(re > 0) {
            update_inode_size(f->inode_info, offset + re);
        }
    } else {
        re = libc_pwritev(fd, iov, iovcnt, offset);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, iovcnt:%d, offset:%ld, re:%d\n", __func__, is_cfs, fd, fd_path, iovcnt, offset, re);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_WRITE, start.tv_sec, start.tv_nsec);
    }
    return re;
}

off_t real_lseek(int fd, off_t offset, int whence) {
    if(fd < 0) {
        return -1;
    }
    off_t re = -1, re_cfs = -1;
    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_lseek(fd, offset, whence);
            if(re < 0) {
                goto log;
            }
        }
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        if(whence == SEEK_SET) {
            f->pos = offset;
        } else if(whence == SEEK_CUR) {
            f->pos += offset;
        } else if(whence == SEEK_END) {
            f->pos = f->inode_info->size + offset;
        }
        re_cfs = f->pos;
        if(strlen(g_client_info.replicate_path) > 0 && re_cfs != re) {
            const char *fd_path = get_fd_path(fd);
            log_debug("hook %s, re from CFS and local is not consistent. is_cfs:%d, fd:%d, path:%s, offset:%ld, whence:%d, re:%d, re_cfs:%d\n", __func__, is_cfs, fd, fd_path, offset, whence, re, re_cfs);
        }
        re = re_cfs;
    } else {
        re = libc_lseek(fd, offset, whence);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, offset:%ld, whence:%d, re:%d\n", __func__, is_cfs, fd, fd_path, offset, whence, re);
    #endif
    return re;
}

ssize_t real_sendfile(int out_fd, int in_fd, off_t *offset, size_t count) {
    if(in_fd < 0 || out_fd < 0) {
        return -1;
    }

    bool is_cfs = fd_in_cfs(in_fd) || fd_in_cfs(out_fd);
    ssize_t re = -1;
    void *buf = NULL;
    if(g_hook && is_cfs) {
        buf = malloc(count);
        if(buf == NULL) {
            goto log;
        }
        if(offset == NULL) {
            re = real_read(in_fd, buf, count);
        } else {
            re = real_pread(in_fd, buf, count, *offset);
        }
        if(re < 0) {
            goto log;
        }
        re = real_write(out_fd, buf, count);
    } else {
        re = libc_sendfile(out_fd, in_fd, offset, count);
    }

log:
    free(buf);
    #ifdef _CFS_DEBUG
    log_debug("hook %s, is_cfs:%d, in_fd:%d, out_fd:%d, offset:%ld, count:%d, re:%d\n", __func__, is_cfs, in_fd, out_fd, offset == NULL ? -1 : *offset, count, re);
    #endif
    return re;
}


/*
 * Synchronized I/O
 */

int real_fdatasync(int fd) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    int re = -1;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_fdatasync(fd);
            if(re < 0) {
                goto log;
            }
        }
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        int re_flush = 0;
        re_flush = flush_inode(f->inode_info);
        re = cfs_errno(cfs_flush(g_client_info.cfs_client_id, fd));
        if(re == 0) {
            re = re_flush;
        }
    } else {
        re = libc_fdatasync(fd);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, re:%d\n", __func__, is_cfs, fd, fd_path, re);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_FLUSH, start.tv_sec, start.tv_nsec);
    }
    return re;
}

int real_fsync(int fd) {
    if(fd < 0) {
        return -1;
    }
    struct timespec start, stop;
    clock_gettime(CLOCK_REALTIME, &start);
    int re = -1;

    bool is_cfs = fd_in_cfs(fd);
    if(g_hook && is_cfs) {
        fd = get_cfs_fd(fd);
        if(strlen(g_client_info.replicate_path) > 0) {
            re = libc_fsync(fd);
            if(re < 0) {
                goto log;
            }
        }
        file_t *f = get_open_file(fd);
        if(f == NULL)
            goto log;
        int re_flush = 0;
        re_flush = flush_inode(f->inode_info);
        re = cfs_errno(cfs_flush(g_client_info.cfs_client_id, fd));
        if(re == 0) {
            re = re_flush;
        }
    } else {
        re = libc_fsync(fd);
    }

log:
    #ifdef _CFS_DEBUG
    ; // labels can only be followed by statements
    const char *fd_path = get_fd_path(fd);
    log_debug("hook %s, is_cfs:%d, fd:%d, path:%s, re:%d\n", __func__, is_cfs, fd, fd_path, re);
    #endif
    if(g_hook && is_cfs) {
        cfs_ump(g_client_info.cfs_client_id, UMP_CFS_FLUSH, start.tv_sec, start.tv_nsec);
    }
    return re;
}

void flush_logs() {
    cfs_flush_log();
}

/*
// DON'T hook signal register function, segfault would occur when calling go
// function in signal handlers. Golang runtime would panic at runtime.cgocallback_gofunc.
int sigaction(int signum, const struct sigaction *act, struct sigaction *oldact) {
    // can't call cfs_init to initialize libc_sigaction, otherwise will be blocked in cfs_new_client
    libc_sigaction = (sigaction_t)dlsym(RTLD_NEXT, "sigaction");
    int re;
    // only hook signals which may terminate process
    bool is_fatal = signum == SIGSEGV || signum == SIGABRT || signum == SIGBUS ||
        signum == SIGILL || signum == SIGFPE || signum == SIGTERM;
    bool hook_action = act != NULL && act->sa_handler != SIG_IGN && is_fatal;
    if(!hook_action) {
        re = libc_sigaction(signum, act, oldact);
        goto log;
    }

    g_sa_handler[signum] = act->sa_handler;
    struct sigaction new_act = {
        .sa_handler = &signal_handler,
        .sa_mask = act->sa_mask,
        .sa_flags = act->sa_flags,
        .sa_restorer = act->sa_restorer
    };
    re = libc_sigaction(signum, &new_act, oldact);

log:
    #ifdef _CFS_DEBUG
    log_debug("hook %s, hook_action:%d, signum:%d, re:%d\n", __func__, hook_action, signum, re);
    #endif
    return re;
}
*/

static void* plugin_open(const char* name) {
    void *handle = dlopen(name, RTLD_NOW|RTLD_GLOBAL);
    #ifdef DYNAMIC_UPDATE
    if(handle != NULL) {
        void* task = dlsym(handle, "main..inittask");
        InitModule_t initModule = (InitModule_t)dlsym(handle, "InitModule");
        initModule(task);
    }
    #endif
    return handle;
}

static int plugin_close(void* handle) {
    FinishModule_t finishModule = (FinishModule_t)dlsym(handle, "FinishModule");
    void* task = dlsym(handle, "main..finitask");
    finishModule(task);
    return dlclose(handle);
}

static void init_cfs_func(void *handle) {
    cfs_sdk_init = (cfs_sdk_init_func)dlsym(handle, "cfs_sdk_init");
    cfs_sdk_close = (cfs_sdk_close_t)dlsym(handle, "cfs_sdk_close");
    cfs_new_client = (cfs_new_client_t)dlsym(handle, "cfs_new_client");
    cfs_close_client = (cfs_close_client_t)dlsym(handle, "cfs_close_client");
    cfs_sdk_state = (cfs_sdk_state_t)dlsym(handle, "cfs_sdk_state");
    cfs_flush_log = (cfs_flush_log_t)dlsym(handle, "cfs_flush_log");
    cfs_ump = (cfs_ump_t)dlsym(handle, "cfs_ump");

    cfs_close = (cfs_close_t)dlsym(handle, "cfs_close");
    cfs_open = (cfs_open_t)dlsym(handle, "cfs_open");
    cfs_openat = (cfs_openat_t)dlsym(handle, "cfs_openat");
    cfs_openat_fd = (cfs_openat_fd_t)dlsym(handle, "cfs_openat_fd");
    cfs_rename = (cfs_rename_t)dlsym(handle, "cfs_rename");
    cfs_renameat = (cfs_renameat_t)dlsym(handle, "cfs_renameat");
    cfs_truncate = (cfs_truncate_t)dlsym(handle, "cfs_truncate");
    cfs_ftruncate = (cfs_ftruncate_t)dlsym(handle, "cfs_ftruncate");
    cfs_fallocate = (cfs_fallocate_t)dlsym(handle, "cfs_fallocate");
    cfs_posix_fallocate = (cfs_posix_fallocate_t)dlsym(handle, "cfs_posix_fallocate");
    cfs_flush = (cfs_flush_t)dlsym(handle, "cfs_flush");
    cfs_get_file = (cfs_get_file_t)dlsym(handle, "cfs_get_file");

    cfs_chdir = (cfs_chdir_t)dlsym(handle, "cfs_chdir");
    cfs_fchdir = (cfs_fchdir_t)dlsym(handle, "cfs_fchdir");
    cfs_getcwd = (cfs_getcwd_t)dlsym(handle, "cfs_getcwd");
    cfs_mkdirs = (cfs_mkdirs_t)dlsym(handle, "cfs_mkdirs");
    cfs_mkdirsat = (cfs_mkdirsat_t)dlsym(handle, "cfs_mkdirsat");
    cfs_rmdir = (cfs_rmdir_t)dlsym(handle, "cfs_rmdir");
    cfs_getdents = (cfs_getdents_t)dlsym(handle, "cfs_getdents");

    cfs_link = (cfs_link_t)dlsym(handle, "cfs_link");
    cfs_linkat = (cfs_linkat_t)dlsym(handle, "cfs_linkat");
    cfs_symlink = (cfs_symlink_t)dlsym(handle, "cfs_symlink");
    cfs_symlinkat = (cfs_symlinkat_t)dlsym(handle, "cfs_symlinkat");
    cfs_unlink = (cfs_unlink_t)dlsym(handle, "cfs_unlink");
    cfs_unlinkat = (cfs_unlinkat_t)dlsym(handle, "cfs_unlinkat");
    cfs_readlink = (cfs_readlink_t)dlsym(handle, "cfs_readlink");
    cfs_readlinkat = (cfs_readlinkat_t)dlsym(handle, "cfs_readlinkat");

    cfs_stat = (cfs_stat_t)dlsym(handle, "cfs_stat");
    cfs_stat64 = (cfs_stat64_t)dlsym(handle, "cfs_stat64");
    cfs_lstat = (cfs_lstat_t)dlsym(handle, "cfs_lstat");
    cfs_lstat64 = (cfs_lstat64_t)dlsym(handle, "cfs_lstat64");
    cfs_fstat = (cfs_fstat_t)dlsym(handle, "cfs_fstat");
    cfs_fstat64 = (cfs_fstat64_t)dlsym(handle, "cfs_fstat64");
    cfs_fstatat = (cfs_fstatat_t)dlsym(handle, "cfs_fstatat");
    cfs_fstatat64 = (cfs_fstatat64_t)dlsym(handle, "cfs_fstatat64");
    cfs_chmod = (cfs_chmod_t)dlsym(handle, "cfs_chmod");
    cfs_fchmod = (cfs_fchmod_t)dlsym(handle, "cfs_fchmod");
    cfs_fchmodat = (cfs_fchmodat_t)dlsym(handle, "cfs_fchmodat");
    cfs_chown = (cfs_chown_t)dlsym(handle, "cfs_chown");
    cfs_lchown = (cfs_lchown_t)dlsym(handle, "cfs_lchown");
    cfs_fchown = (cfs_fchown_t)dlsym(handle, "cfs_fchown");
    cfs_fchownat = (cfs_fchownat_t)dlsym(handle, "cfs_fchownat");
    cfs_futimens = (cfs_futimens_t)dlsym(handle, "cfs_futimens");
    cfs_utimens = (cfs_utimens_t)dlsym(handle, "cfs_utimens");
    cfs_utimensat = (cfs_utimensat_t)dlsym(handle, "cfs_utimensat");
    cfs_access = (cfs_access_t)dlsym(handle, "cfs_access");
    cfs_faccessat = (cfs_faccessat_t)dlsym(handle, "cfs_faccessat");

    cfs_setxattr = (cfs_setxattr_t)dlsym(handle, "cfs_setxattr");
    cfs_lsetxattr = (cfs_lsetxattr_t)dlsym(handle, "cfs_lsetxattr");
    cfs_fsetxattr = (cfs_fsetxattr_t)dlsym(handle, "cfs_fsetxattr");
    cfs_getxattr = (cfs_getxattr_t)dlsym(handle, "cfs_getxattr");
    cfs_lgetxattr = (cfs_lgetxattr_t)dlsym(handle, "cfs_lgetxattr");
    cfs_fgetxattr = (cfs_fgetxattr_t)dlsym(handle, "cfs_fgetxattr");
    cfs_listxattr = (cfs_listxattr_t)dlsym(handle, "cfs_listxattr");
    cfs_llistxattr = (cfs_llistxattr_t)dlsym(handle, "cfs_llistxattr");
    cfs_flistxattr = (cfs_flistxattr_t)dlsym(handle, "cfs_flistxattr");
    cfs_removexattr = (cfs_removexattr_t)dlsym(handle, "cfs_removexattr");
    cfs_lremovexattr = (cfs_lremovexattr_t)dlsym(handle, "cfs_lremovexattr");
    cfs_fremovexattr = (cfs_fremovexattr_t)dlsym(handle, "cfs_fremovexattr");

    cfs_fcntl = (cfs_fcntl_t)dlsym(handle, "cfs_fcntl");
    cfs_fcntl_lock = (cfs_fcntl_lock_t)dlsym(handle, "cfs_fcntl_lock");
    cfs_alloc_fd = (cfs_alloc_fd_t)dlsym(handle, "cfs_alloc_fd");

    cfs_read = (cfs_read_t)dlsym(handle, "cfs_read");
    cfs_pread = (cfs_pread_t)dlsym(handle, "cfs_pread");
    cfs_readv = (cfs_readv_t)dlsym(handle, "cfs_readv");
    cfs_preadv = (cfs_preadv_t)dlsym(handle, "cfs_preadv");
    cfs_write = (cfs_write_t)dlsym(handle, "cfs_write");
    cfs_pwrite = (cfs_pwrite_t)dlsym(handle, "cfs_pwrite");
    cfs_pwrite_inode = (cfs_pwrite_inode_t)dlsym(handle, "cfs_pwrite_inode");
    cfs_writev = (cfs_writev_t)dlsym(handle, "cfs_writev");
    cfs_pwritev = (cfs_pwritev_t)dlsym(handle, "cfs_pwritev");
    cfs_lseek = (cfs_lseek_t)dlsym(handle, "cfs_lseek");
    cfs_read_requests = (cfs_read_requests_t)dlsym(handle, "cfs_read_requests");
    cfs_refresh_eks = (cfs_refresh_eks_t)dlsym(handle, "cfs_refresh_eks");
}

int start_libs(void *args) {
    #ifdef _CFS_DEBUG
    printf("constructor\n");
    #endif

    int res = -1;
    int *dup_fds;
    char *mount_point;
    const char *config_path;
    const char *libcfssdk_path;
    client_config_t client_config;
    client_state_t *client_state;
    client_state_t null_state = {NULL, NULL, 0, NULL, 0, NULL, false};
    if (args != NULL) {
        client_state = (client_state_t *)args;
    } else {
        client_state = &null_state;
    }

    config_path = getenv("CFS_CONFIG_PATH");
    libcfssdk_path = getenv("CFS_CFSSDK_PATH");
    g_client_info.sdk_handle = plugin_open(libcfssdk_path);
    if(g_client_info.sdk_handle == NULL) {
        fprintf(stderr, "dlopen %s error: %s.\n", libcfssdk_path, dlerror());
        goto out;
    }
    init_cfs_func(g_client_info.sdk_handle);

    // parse client configurations from ini file.
    memset(&client_config, 0, sizeof(client_config_t));
    // libc printf CANNOT be used in this init function, otherwise will cause circular dependencies.
    if(ini_parse(config_path, config_handler, &client_config) < 0) {
        fprintf(stderr, "Can't load CFS config file, use CFS_CONFIG_PATH env variable.\n");
        goto out;
    }

    if(client_config.mount_point == NULL || client_config.log_dir == NULL) {
        fprintf(stderr, "Check CFS config file for mountPoint or logDir.\n");
        goto out;
    }

    g_client_info.ignore_path = client_config.ignore_path;
    if(g_client_info.ignore_path == NULL) {
        g_client_info.ignore_path = "";
    }
    g_client_info.replicate_path = client_config.replicate_path;
    if(g_client_info.replicate_path == NULL) {
        g_client_info.replicate_path = "";
    }
    if(strlen(g_client_info.replicate_path) > 0) {
        #define DUP_TO_LOCAL
    }

    mount_point = getenv("CFS_MOUNT_POINT");
    if(mount_point != NULL)
        g_client_info.mount_point = get_clean_path(mount_point);
    else
        g_client_info.mount_point = get_clean_path(client_config.mount_point);
    if(g_client_info.mount_point[0] != '/') {
        fprintf(stderr, "Mount point %s is not an absolute path.\n", g_client_info.mount_point);
        goto out;
    }
    free(client_config.mount_point);

    cfs_sdk_init_t init_config;
    init_config.ignore_sighup = 1;
    init_config.ignore_sigterm = 1;
    init_config.log_dir = client_config.log_dir;
    init_config.log_level = client_config.log_level;
    init_config.prof_port = client_config.prof_port;
    if(cfs_sdk_init(&init_config) != 0) {
        fprintf(stderr, "Can't initialize CFS SDK, check output.log for details.\n");
        goto out;
    }
    free(client_config.log_dir);
    free(client_config.log_level);
    free(client_config.prof_port);

    pthread_rwlock_init(&g_client_info.dup_fds_lock, NULL);
    pthread_rwlock_init(&g_client_info.open_files_lock, NULL);
    pthread_rwlock_init(&g_client_info.open_inodes_lock, NULL);
    g_client_info.cwd = client_state->cwd;
    g_client_info.in_cfs = client_state->in_cfs;
    g_client_info.stop = false;
    g_client_info.has_renameat2 = has_renameat2();
    g_client_info.big_page_cache = new_lru_cache(BIG_PAGE_CACHE_SIZE, BIG_PAGE_SIZE);
    g_client_info.small_page_cache = new_lru_cache(SMALL_PAGE_CACHE_SIZE, SMALL_PAGE_SIZE);
    if(g_client_info.big_page_cache == NULL || g_client_info.small_page_cache == NULL) {
        goto out;
    }
    g_client_info.conn_pool = new_conn_pool();

    g_client_info.cfs_client_id = cfs_new_client(NULL, config_path, client_state->sdk_state);
    if(g_client_info.cfs_client_id < 0) {
        fprintf(stderr, "Can't start CFS client, check output.log for details.\n");
        goto out;
    }

    dup_fds = client_state->dup_fds;
    for(int i = 0; i < client_state->fd_num; i = i+2) {
        g_client_info.dup_fds[dup_fds[i]] = dup_fds[i+1];
    }
    for(int i = 0; i < client_state->file_num; i++) {
        res = record_open_file(client_state->files+i);
        if(res < 0) {
            fprintf(stderr, "rebuild open_file failed.\n");
            goto out;
        }
    }

    g_client_info.inode_wrapper = {&g_client_info.open_inodes_lock, &g_client_info.open_inodes, &g_client_info.stop};
    pthread_create(&g_client_info.bg_pthread, NULL, do_flush_inode, &g_client_info.inode_wrapper);
    res = 0;
out:
    if(client_state->sdk_state != NULL) free(client_state->sdk_state);
    if(client_state->files != NULL) free(client_state->files);
    if(client_state->dup_fds != NULL) free(client_state->dup_fds);
    if(args != NULL) free(args);
    return res;
}

void* stop_libs() {
    fprintf(stderr, "Begin to update sdk.\n");
    int res;
    size_t size;
    char *sdk_state = NULL;
    int *dup_fds = NULL;
    cfs_file_t *files = NULL;
    client_state_t *client_state = NULL;

    g_client_info.stop = true;
    pthread_join(g_client_info.bg_pthread, NULL);
    fprintf(stderr, "pthread do_flush_inode stopped.\n");

    client_state = (client_state_t *)malloc(sizeof(client_state_t));
    if(client_state == NULL) {
        fprintf(stderr, "malloc client_state_t failed.\n");
        goto err;
    }
    memset(client_state, '\0', sizeof(client_state_t));
    client_state->cwd = g_client_info.cwd;
    client_state->in_cfs = g_client_info.in_cfs;

    if(g_client_info.dup_fds.size() > 0) {
        int count = 0;
        dup_fds = (int*) calloc(g_client_info.dup_fds.size() * 2, sizeof(int));
        if(dup_fds == NULL) {
            fprintf(stderr, "calloc client_state->dup_fds failed.\n");
            goto err;
        }
        for (auto it = g_client_info.dup_fds.begin(); it != g_client_info.dup_fds.end(); it++) {
            dup_fds[count] = it->first;
            dup_fds[count + 1] = it->second;
            count += 2;
        }
        client_state->dup_fds = dup_fds;
        client_state->fd_num = count;
    }

    if(g_client_info.open_files.size() > 0) {
        int count = 0;
        files = (cfs_file_t *) calloc(g_client_info.open_files.size(), sizeof(cfs_file_t));
        if(files == NULL) {
            fprintf(stderr, "calloc client_state->files failed.\n");
            goto err;
        }
        for(auto it = g_client_info.open_files.begin(); it != g_client_info.open_files.end(); it++) {
            file_t *f = it->second;
            (files+count)->fd = f->fd;
            (files+count)->flags = f->flags;
            (files+count)->file_type = f->file_type;
            (files+count)->dup_ref = f->dup_ref;
            (files+count)->pos = f->pos;
            (files+count)->inode = f->inode_info->inode;
            (files+count)->size = f->inode_info->size;
            count++;
            free(f);
        }
        client_state->files = files;
        client_state->file_num = count;
    }

    flush_and_release(g_client_info.open_inodes);
    release_lru_cache(g_client_info.big_page_cache);
    release_lru_cache(g_client_info.small_page_cache);
    release_conn_pool(g_client_info.conn_pool);
    size = cfs_sdk_state(g_client_info.cfs_client_id, NULL, 0);
    if (size > 0) {
        sdk_state = (char *)malloc(size);
        if(sdk_state == NULL) {
            fprintf(stderr, "malloc sdk_state failed, size\n: %d.", size);
            goto err;
        }
        memset(sdk_state, '\0', size);
        cfs_sdk_state(g_client_info.cfs_client_id, sdk_state, size);
    }
    client_state->sdk_state = sdk_state;

    cfs_sdk_close();
    res = plugin_close(g_client_info.sdk_handle);
    fprintf(stderr, "finish dlclose sdk.\n");
    if (res != 0) {
        fprintf(stderr, "dlclose libcfssdk.so error: %s\n", dlerror());
        goto err;
    }
    free((void*)g_client_info.mount_point);
    if(strlen(g_client_info.ignore_path) > 0) {
        free((void*)g_client_info.ignore_path);
    }
    return client_state;

err:
    if(client_state != NULL) free(client_state);
    if(sdk_state != NULL) free(sdk_state);
    if(dup_fds != NULL) free(dup_fds);
    if(files != NULL) free(files);
    return NULL;
}
