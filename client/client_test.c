#define _GNU_SOURCE
#include <assert.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/sendfile.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

//LD_PRELOAD=libcfsclient.so MOUNT_POINT=/export/data/mysql ./a.out

#define clean_errno() (errno == 0 ? "None" : strerror(errno))
#define log_error(M, ...) fprintf(stderr, "[ERROR] (%s:%d: errno: %s) " M "\n", __FILE__, __LINE__, clean_errno(), ##__VA_ARGS__)
#define assertf(A, M, ...) if(!(A)) {log_error(M, ##__VA_ARGS__); assert(A); }

void testOp(const char *file);
void testReload();
void testDup();
void testUnlinkAndRename();
void testSymlink();

#define PATH_LEN 100
bool is_cfs;
char* mount;

int main(int argc, char **argv) {
    int num = 1;
    int c;
    while((c = getopt(argc, argv, "lin:h")) != -1)
    switch(c) {
        case 'n':
        num = atoi(optarg);
        break;
        case 'h':
        printf("There are three test modes: local, CFS(use LD_PRELOAD).\n-n num\n  execute num times (default 1)\n");
        return 0;
    }
    is_cfs = getenv("LD_PRELOAD");
    mount = getenv("MOUNT_POINT");
    if(mount == NULL) {
        printf("execute with MOUNT_POINT=\n");
        return -1;
    }

    time_t raw_time;
    struct tm *ptm;
    char buf[20];
    const int count = is_cfs ? 100 : 100000;
    for(int i = 0; i < num; i++) {
        testOp("tmp123");
        if(i >= count && i % count == 0) {
            raw_time = time(NULL);
            ptm = localtime(&raw_time);
            strftime(buf, 20, "%F %H:%M:%S", ptm);
            printf("%s testOp for %d times\n", buf, i);
        }
    }
    printf("Finish testOp for %d times.\n", num);
    if(is_cfs) {
        testReload();
        setenv("MOUNT_POINT", mount, 1);
    }
    testDup();
    printf("Finish testDup\n");
    testUnlinkAndRename();
    printf("Finish test unlink and rename\n");
    testSymlink();
    printf("Finish test symlink\n");
    printf("Finish all tests.\n");
    return 0;
}

void testReload() {
    printf("Test update libcfssdk.so. Please waiting finish...\n");
    setenv("RELOAD_CLIENT", "test", 1);
    sleep(30);
    printf("finish client update.\n");
}

void testOp(const char *file) {
    char cwd[PATH_LEN] = {0};           // root for this test
    char dir[PATH_LEN] = {0};           // temp dir
    char path[PATH_LEN] = {0};          // reame source file
    char path_sendfile[PATH_LEN] = {0}; // sendfile source file
    char new_path[PATH_LEN] = {0};      // rename to file
    const char *tdir = "t";
    const char *new_file = "tmp1234";
    strcat(cwd, mount);
    sprintf(dir, "%s/%s", cwd, tdir);
    sprintf(path, "%s/%s", dir, file);
    sprintf(path_sendfile, "%s/%s1", dir, file);
    sprintf(new_path, "%s/%s", dir, new_file);

    #define LEN 2
    char wbuf[LEN] = "a";
    char rbuf[LEN] = {0};
    int dir_fd, fd, tmp_fd, re;
    ssize_t size;
    off_t off;

    unlink(path);
    rmdir(dir);

    // chdir operations
    char tmp_buf[PATH_LEN] = {0};
    //buf is not enough for the cwd
    char *tmp_dir = getcwd(tmp_buf, 1);
    assertf(tmp_dir == NULL, "getcwd returing %s", tmp_dir);
    tmp_dir = getcwd(tmp_buf, PATH_LEN);
    assertf(tmp_dir == tmp_buf, "getcwd returing invalid poiter");
    re = mkdir(dir, 0775);
    assertf(re == 0, "mkdir %s returning %d", dir, re);
    dir_fd = open(dir, O_RDONLY | O_DIRECTORY);
    assertf(dir_fd > 0, "open dir %s returning %d", dir, dir_fd);
    re = chdir(cwd);
    tmp_dir = getcwd(NULL, 0);
    assertf(re == 0 && !strcmp(cwd, tmp_dir), "chdir %s returning %d %s", cwd, re, tmp_dir);
    free(tmp_dir);
    re = chdir(tdir);
    tmp_dir = getcwd(NULL, 0);
    assertf(re == 0 && !strcmp(dir, tmp_dir), "chdir %s returning %d %s", tmp_dir, re, tmp_dir);
    free(tmp_dir);
    tmp_dir = getcwd(NULL, PATH_LEN);
    assertf(tmp_dir != NULL && !strcmp(tmp_dir, dir), "getcwd returning %s, len: %d, expect: %s", tmp_dir, strlen(tmp_dir), dir);
    free(tmp_dir);
    re = fchdir(dir_fd);
    assertf(re == 0, "fchdir %d returning %d", dirfd, re);
    tmp_dir = getcwd(NULL, PATH_LEN);
    assertf(tmp_dir != NULL && !strcmp(tmp_dir, dir), "getcwd returning %s, len: %d", tmp_dir, strlen(tmp_dir));
    free(tmp_dir);

    // readdir operations
    fd = openat(dir_fd, file, O_RDWR | O_CREAT, 0664);
    assertf(fd > 0, "openat %s returning %d", path, fd);
    close(fd);
    fd = openat(dir_fd, file, O_RDWR | O_CREAT | O_EXCL);
    assertf(fd == -1 && errno == EEXIST, "openat %s returning %d", path, fd);
    DIR *dirp = fdopendir(dir_fd);
    assertf(dirp != NULL, "fdopendir %s returning NULL", dir);
    re = closedir(dirp);
    assertf(re == 0, "closedir returning %d", re);
    dir_fd = open(dir, O_RDONLY | O_DIRECTORY);
    assertf(dir_fd > 0, "open dir %s returning %d", dir, dir_fd);
    dirp = opendir(dir);
    assertf(dirp != NULL, "opendir %s returning NULL", dir);
    struct dirent *dp;
    while((dp = readdir(dirp)) && (!strcmp(dp->d_name, ".") || !strcmp(dp->d_name, "..")));
    assertf(dp != NULL && !strcmp(dp->d_name, file), "readdir returning %s", dp == NULL ? "NULL" : dp->d_name);
    dp = readdir(dirp);
    assertf(dp == NULL, "readdir errno %d", errno);
    re = closedir(dirp);
    assertf(re == 0, "closedir returning %d", re);
    // donot close dirp to keep dir_fd open for following use
    dirp = fdopendir(dir_fd);
    assertf(dirp != NULL, "fdopendir %s returning NULL", dir);
    while((dp = readdir(dirp)) && (!strcmp(dp->d_name, ".") || !strcmp(dp->d_name, "..")));
    assertf(dp != NULL && !strcmp(dp->d_name, file), "readdir returning %s", dp == NULL ? "NULL" : dp->d_name);
    dp = readdir(dirp);
    assertf(dp == NULL, "readdir errno %d", errno);

    // file operations
    fd = open(file, O_RDWR);
    assertf(fd > 0, "open %s returning %d", path, fd);
    re = renameat(dir_fd, file, dir_fd, new_file);
    assertf(re == 0, "renameat firfd %d %s to %s returning %d", dirfd, file, new_file, re);
    tmp_fd = open(path, O_RDONLY);
    assertf(tmp_fd < 0, "open %s after rename with O_RDONLY returning %d", path, tmp_fd);
    re = rename(new_path, path);
    assertf(re == 0, "rename %s to %s returning %d", new_path, path, re);
    re = truncate(path, 123);
    assertf(re == 0, "truncate %s returning %d", path, re);
    struct stat statbuf;
    re = stat(path, &statbuf);
    assertf(re == 0 && statbuf.st_size == 123, "stat %s returning %d, size: %d", path, re, statbuf.st_size);
    re = ftruncate(fd, 0);
    assertf(re == 0, "ftruncate %d returning %d", fd, re);
    re = stat(path, &statbuf);
    assertf(re == 0 && statbuf.st_size == 0, "stat %s returning %d, size: %d", path, re, statbuf.st_size);

    // read & write
    size = write(fd, wbuf, LEN-1);
    assertf(size == LEN-1, "write %s to %s returning %d", wbuf, path, size);
    size = read(fd, rbuf, LEN-1);
    assertf(size == 0, "read %s from %s after write returning %d", rbuf, path, size);
    off = lseek(fd, 0, SEEK_SET);
    assertf(off == 0, "lseek returning %d", off);
    size = read(fd, rbuf, LEN-1);
    assertf(size == LEN-1 && strncmp(wbuf, rbuf, LEN-1) == 0,
            "read %s from %s after write returning %d", rbuf, path, size);
    size = pwrite(fd, wbuf, LEN-1, LEN-1);
    assertf(size == LEN-1, "write %s to %s at offset %d return %d", wbuf, path, LEN-1, size);
    size = pread(fd, rbuf, LEN-2, LEN);
    assertf(size == LEN-2 && strncmp(wbuf+1, rbuf, LEN-2) == 0, "pread %s from %s at offset %d returning %d", rbuf, path, LEN-1, size);

    // sendfile
    tmp_fd = open(path_sendfile, O_RDWR | O_CREAT, 0664);
    assertf(tmp_fd > 0, "open %s returning %d", path_sendfile, tmp_fd);
    off = lseek(fd, 0, SEEK_SET);
    assertf(off == 0, "lseek returning %d", off);
    size = sendfile(tmp_fd, fd, NULL, LEN-1);
    assertf(size == LEN-1, "sendfile from %d to %d returning %d", fd, tmp_fd, size);

    // file attributes
    // CFS time precision is second, tv_nsec should be 0
    struct timespec ts[2] = {{1605668000, 0}, {1605668001, 0}};
    re = utimensat(dir_fd, file, ts, 0);
    assertf(re == 0, "utimensat %s at dir fd %d returning %d", file, dir_fd, re);
    re = chmod(path, 0611);
    assertf(re == 0, "chmod %s returning %d", path, re);
    re = stat(path, &statbuf);
    // access time is updated in metanode when accessing inode, inconsistent with client inode cache
    bool atim_valid = is_cfs ?
        ts[0].tv_sec <= statbuf.st_atime:
        !memcmp((void*)&ts[0].tv_sec, (void*)&statbuf.st_atime, sizeof(time_t));
    assertf(re == 0 && statbuf.st_size == 2*LEN-2
            && atim_valid
            && !memcmp((void*)&ts[1].tv_sec, (void*)&statbuf.st_mtime, sizeof(time_t))
            && statbuf.st_mode == S_IFREG | 0611,
            "stat %s returning %d, size: %d, mode: %o", path, re, statbuf.st_size, statbuf.st_mode);

    // chdir to original cwd, in case of calling test() for many times
    re = chdir(cwd);
    assertf(re == 0, "chdir %s returning %d", cwd, re);
    tmp_dir = getcwd(NULL, PATH_LEN);
    assertf(tmp_dir != NULL && !strcmp(tmp_dir, cwd), "getcwd returning %s, len: %d", tmp_dir, strlen(tmp_dir));
    free(tmp_dir);

    // cleaning
    re = close(dir_fd);
    assertf(re == 0, "close dir fd %d returning %d", fd, re);
    re = close(fd);
    assertf(re == 0, "close fd %d returning %d", fd, re);
    re = close(tmp_fd);
    assertf(re == 0, "close fd %d returning %d", tmp_fd, re);
    re = lseek(fd, 0, SEEK_SET);
    assertf(re < 0, "lseek closed fd %d returning %d", fd, re);
    re = unlink(path);
    assertf(re == 0, "unlink %s returning %d", path, re);
    re = unlink(path_sendfile);
    assertf(re == 0, "unlink %s returning %d", path_sendfile, re);
    tmp_fd = open(path, O_RDONLY);
    assertf(tmp_fd < 0, "open unlinked %s wirt O_RDONLY returning %d", path, tmp_fd);
    re = rmdir(dir);
    assertf(re == 0, "rmdir %s returning %d", dir, re);
    dir_fd = open(dir, O_RDONLY | O_DIRECTORY);
    assertf(dir_fd < 0, "open removed dir %s returning %d", dir, dir_fd);
}

void testDup() {
    char *path = "dir";
    char *file = "file1";
    off_t off;
    int dirfd, dirfd1, fd, newfd1, newfd2, newfd3;
    ssize_t size;
    int res;

    char dir[PATH_LEN] = {0};
    strcat(dir, mount);
    strcat(dir, "/");
    strcat(dir, path);

    char filepath[PATH_LEN] = {0};
    strcat(filepath, dir);
    strcat(filepath, "/");
    strcat(filepath, file);
    unlink(filepath);
    rmdir(dir);

    res = mkdir(dir, 0775);
    assertf(res == 0, "mkdir %s returning %d", dir, res);
    dirfd = open(dir, O_RDONLY | O_DIRECTORY);
    assertf(dirfd > 0, "open dir %s returning %d", dir, dirfd);
    dirfd1 = dup2(dirfd, 99);
    assertf(dirfd1 > 0, "dup2 fd %d returning %d, expect 99", dirfd, dirfd1);
    res = close(dirfd);
    assertf(res == 0, "close fd %d returning %d, expect 0", dirfd, res);
    fd = openat(dirfd1, file, O_RDWR | O_CREAT, 0664);
    assertf(fd > 0, "open %s/dir/file1 returning %d", mount, fd);

    size = write(fd, "test", 4);
    assertf(size == 4, "write test to fd returning %d, expect 4", size);
    newfd1 = dup(fd);
    assertf(newfd1 > 0, "dup fd %d returning %d", fd, newfd1);
    off = lseek(newfd1, 0, SEEK_CUR);
    assertf(off == 4, "lseek returning %d, expect 4", off);
    newfd2 = dup2(fd, 100);
    assertf(newfd2 == 100, "dup2 fd %d returning %d, expect 100", fd, newfd2);
    off = lseek(newfd2, 0, SEEK_CUR);
    assertf(off == 4, "lseek returning %d, expect 4", off);

    res = close(fd);
    assertf(res == 0, "close fd %d returning %d, expect 0", fd, res);

    newfd3 = fcntl(newfd2, F_DUPFD, 200);
    assertf(newfd3 >= 200, "fcntl dup fd %d returning %d, expect 200", fd, newfd1);
    size = write(newfd1, "test", 4);
    assertf(size == 4, "write test to fd returning %d, expect 4", size);
    size = write(newfd2, "test", 4);
    assertf(size == 4, "write test to fd returning %d, expect 4", size);
    size = write(newfd3, "test", 4);
    assertf(size == 4, "write test to fd returning %d, expect 4", size);

    off = lseek(newfd1, 0, SEEK_CUR);
    assertf(off == 16, "lseek returning %d, expect 4", off);
    res = close(newfd1);
    assertf(res == 0, "close fd %d returning %d, expect 0", newfd1, res);

    size = write(newfd2, "test", 4);
    assertf(size == 4, "write test to fd returning %d, expect 4", size);
    res = close(newfd2);
    assertf(res == 0, "close fd %d returning %d, expect 0", newfd1, res);

    size = write(newfd3, "test", 4);
    assertf(size == 4, "write test to fd returning %d, expect 4", size);
    res = close(newfd3);
    assertf(res == 0, "close fd %d returning %d, expect 0", newfd1, res);

    size = write(newfd2, "test", 4);
    assertf(size == -1, "write test to close fd returning %d, expect -1", size);

    res = close(dirfd1);
    assertf(res == 0, "close dir returning %d", res);

    unlink(filepath);
    rmdir(dir);
}

void testUnlinkAndRename() {
    #define COUNT 10
    char *mount = getenv("MOUNT_POINT");
    char *file_1 = "testUnlinkAndRename_1";
    char *file_2 = "testUnlinkAndRename_2";
    int fd_1, fd_2;
    int re, size;
    char *write_buf_1 = malloc(COUNT);
    char *write_buf_2 = malloc(COUNT);
    char *read_buf = malloc(COUNT);
    memset(write_buf_1, '1', COUNT);
    memset(write_buf_2, '2', COUNT);
    memset(read_buf, ' ', COUNT);

    char path_1[PATH_LEN] = {0};
    char path_2[PATH_LEN] = {0};
    strcat(path_1, mount);
    strcat(path_1, "/");
    strcat(path_1, file_1);
    strcat(path_2, mount);
    strcat(path_2, "/");
    strcat(path_2, file_2);

    fd_1 = open(path_1, O_RDWR|O_CREAT|O_TRUNC, 0666);
    assertf(fd_1 > 0, "open file %s returning %d", path_1, fd_1);
    fd_2 = open(path_2, O_RDWR|O_CREAT|O_TRUNC, 0666);
    assertf(fd_2 > 0, "open file %s returning %d", path_2, fd_2);

    size = write(fd_1, write_buf_1, COUNT);
    assertf(size == COUNT, "write file:%s returning %d, expect %d", path_1, size, COUNT);
    size = write(fd_2, write_buf_2, COUNT);
    assertf(size == COUNT, "write file:%s returning %d, expect %d", path_2, size, COUNT);

    re = rename(path_1, path_2);
    assertf(re == 0, "rename from %s to %s failed", path_1, path_2);

    lseek(fd_1, 0, SEEK_SET);
    size = read(fd_1, read_buf, COUNT);
    assertf(size == COUNT, "after rename: read file %s size %d, expect %d", path_1, size, COUNT);
    assertf(memcmp(read_buf, write_buf_1, size) == 0, "after unlink: read file %s failed, read:%s, expect:%s", path_1, read_buf, write_buf_1);

    lseek(fd_2, 0, SEEK_SET);
    size = read(fd_2, read_buf, COUNT);
    assertf(size == COUNT, "after rename: read file %s size %d, expect %d", path_2, size, COUNT);
    assertf(memcmp(read_buf, write_buf_2, size) == 0, "after rename: read file %s failed, read:%s, expect:%s", path_1, read_buf, write_buf_2);

    re = unlink(path_2);
    assertf(re == 0, "unlink file %s failed", path_2);

    lseek(fd_1, 0, SEEK_SET);
    size = read(fd_1, read_buf, COUNT);
    assertf(size == COUNT, "after unlink: read file %s size %d, expect %d", path_1, size, COUNT);
    assertf(memcmp(read_buf, write_buf_1, size) == 0, "after unlink: read file %s failed, read:%s, expect:%s", path_1, read_buf, write_buf_1);

    lseek(fd_2, 0, SEEK_SET);
    size = read(fd_2, read_buf, COUNT);
    assertf(size == COUNT, "after unlink: read file %s size %d, expect %d", path_2, size, COUNT);
    assertf(memcmp(read_buf, write_buf_2, size) == 0, "after unlink: read file %s failed, read:%s, expect:%s", path_1, read_buf, write_buf_2);

    close(fd_1);
    close(fd_2);
}

void testSymlink() {
    char *path = "dir2";
    char *file1 = "file1";
    char *file2 = "file2";
    char *file3 = "notExist";

    char dir[PATH_LEN] = {0};
    sprintf(dir, "%s/%s", mount, path);
    char filepath1[PATH_LEN] = {0};
    sprintf(filepath1, "%s/%s", dir, file1);
    char filepath2[PATH_LEN] = {0};
    sprintf(filepath2, "%s/%s", dir, file2);
    char filepath3[PATH_LEN] = {0};
    sprintf(filepath3, "%s/%s", dir, file3);

    int res, fd;
    ssize_t size;
    char buf[PATH_LEN] = {0};
    struct stat statbuf;

    res = mkdir(dir, 0775);
    assertf(res == 0, "mkdir %s returning %d", dir, res);
    fd = open(filepath1, O_RDWR | O_CREAT, 0664);
    assertf(fd > 0, "open %s returning %d", filepath1, fd);
    size = write(fd, "test", 4);
    assertf(size == 4, "write test to fd returning %d, expect 4", size);
    res = close(fd);
    assertf(res == 0, "close fd %d returning %d, expect 0", fd, res);

    res = symlink(filepath1, filepath2);
    assertf(res == 0, "symlink %s to %s returning %d, expect 0", filepath2, filepath1, res);

    res = access(filepath1, F_OK);
    assertf(res == 0, "access %s returing %d, expect 0", filepath1, res);
    res = access(filepath2, F_OK);
    assertf(res == 0, "access symlink %s returing %d, expect 0", filepath2, res);

    size = readlink(filepath1, buf, PATH_LEN);
    assertf(size == -1, "readlink %s returning %d, expect -1", filepath1, size);
    size = readlink(filepath2, buf, PATH_LEN);
    assertf(size == strlen(filepath1), "readlink symlink %s returning %d, expect %d", filepath2, size, strlen(filepath1));
    assertf(memcmp(buf, filepath1, size) == 0, "readlink symlink %s returning %s, expect %s", filepath2, buf, filepath1);

    res = stat(filepath1, &statbuf);
    assertf(res == 0, "stat %s returning %d, expect 0", filepath1, res);
    res = stat(filepath2, &statbuf);
    assertf(res == 0, "stat symlink %s returning %d, expect 0", filepath2, res);

    char *p = realpath(filepath1, buf);
    assertf(memcmp(p, filepath1, strlen(filepath1)) == 0, "realpath %s returning %s; expect %s", filepath1, p, filepath1);
    p = realpath(filepath2, buf);
    assertf(memcmp(p, filepath1, strlen(filepath1)) == 0, "realpath %s returning %s; expect %s", filepath2, p, filepath1);
    p = realpath(filepath3, buf);
    assertf(errno == ENOENT && p == NULL, "realpath %s returing %s, errno: %d; expect NULL, errno: ENOENT", filepath3, p, errno);

    unlink(filepath2);
    unlink(filepath1);
    rmdir(dir);
}
