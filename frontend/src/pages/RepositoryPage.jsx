import { useState, useEffect } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { groupsAPI, repositoriesAPI, authAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import './RepositoryPage.css';

function RepositoryPage() {
  const { groupName, repoName } = useParams();
  const navigate = useNavigate();
  const [data, setData] = useState(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);

  const loadData = async () => {
    try {
      const decodedGroup = decodeURIComponent(groupName);
      const decodedRepo = decodeURIComponent(repoName);
      const repoData = await groupsAPI.getTags(decodedGroup, decodedRepo);
      setData(repoData);
    } catch (error) {
      console.error('Failed to load repository data:', error);
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  };

  useEffect(() => {
    loadData();
  }, [groupName, repoName]);

  const handleRefresh = () => {
    setIsRefreshing(true);
    loadData();
  };

  const handleLogout = async () => {
    if (window.confirm('Are you sure you want to logout?')) {
      await authAPI.logout();
      navigate('/login');
    }
  };

  const handleDeleteTag = async (tagName) => {
    if (!window.confirm(`Are you sure you want to delete tag "${tagName}"?`)) {
      return;
    }

    try {
      const fullRepoName = `${decodeURIComponent(groupName)}/${decodeURIComponent(repoName)}`;
      await repositoriesAPI.deleteTag(fullRepoName, tagName);
      loadData();
    } catch (error) {
      console.error('Failed to delete tag:', error);
      alert('Failed to delete tag');
    }
  };

  if (isLoading) {
    return (
      <div className="d-flex justify-content-center align-items-center" style={{ minHeight: '100vh' }}>
        <div className="spinner-border text-primary" role="status">
          <span className="visually-hidden">Loading...</span>
        </div>
      </div>
    );
  }

  const decodedGroup = decodeURIComponent(groupName);
  const decodedRepo = decodeURIComponent(repoName);

  return (
    <div className="container-main">
      <nav className="navbar navbar-expand-lg navbar-dark mb-4">
        <div className="container-fluid">
          <Link to="/" className="navbar-brand text-decoration-none">
            <i className="bi bi-box-seam me-2"></i>Refity Docker Registry
          </Link>
          <div className="navbar-nav ms-auto d-flex flex-row gap-2">
            <button
              onClick={handleRefresh}
              className="btn btn-outline-light"
              disabled={isRefreshing}
            >
              {isRefreshing ? (
                <span className="spinner-border spinner-border-sm me-2"></span>
              ) : (
                <i className="bi bi-arrow-clockwise me-1"></i>
              )}
              Refresh
            </button>
            <button onClick={handleLogout} className="btn btn-outline-light">
              <i className="bi bi-box-arrow-right me-1"></i>Logout
            </button>
          </div>
        </div>
      </nav>

      <div className="container-fluid px-0">
        <nav aria-label="breadcrumb" className="mb-4">
          <ol className="breadcrumb">
            <li className="breadcrumb-item">
              <Link to="/">Dashboard</Link>
            </li>
            <li className="breadcrumb-item">
              <Link to={`/group/${encodeURIComponent(decodedGroup)}`}>{decodedGroup}</Link>
            </li>
            <li className="breadcrumb-item active" aria-current="page">
              {decodedRepo}
            </li>
          </ol>
        </nav>

        <div className="card">
          <div className="card-header">
            <h5 className="mb-0">
              <i className="bi bi-box-seam me-2"></i>Repository: {decodedGroup}/{decodedRepo}
            </h5>
            <small className="text-muted">List of tags for this repository</small>
          </div>
          <div className="card-body">
            {data?.tags && data.tags.length > 0 ? (
              <div className="table-responsive">
                <table className="table table-hover">
                  <thead>
                    <tr>
                      <th>Tag</th>
                      <th>Size</th>
                      <th>Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.tags.map((tag) => (
                      <tr key={tag.name}>
                        <td>
                          <span className="badge bg-primary">
                            <i className="bi bi-tag me-1"></i>
                            {tag.name}
                          </span>
                        </td>
                        <td>{formatBytes(tag.size)}</td>
                        <td>
                          <button
                            onClick={() => handleDeleteTag(tag.name)}
                            className="btn btn-sm btn-danger"
                            title="Delete tag"
                          >
                            <i className="bi bi-trash"></i>
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <p className="text-muted mb-0">No tags found for this repository.</p>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

export default RepositoryPage;

